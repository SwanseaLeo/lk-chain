package types

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"sync"
	"time"

	crypto "github.com/tendermint/go-crypto"
	data "github.com/tendermint/go-wire/data"
	cmn "github.com/tendermint/tmlibs/common"
)

// TODO: type ?
const (
	stepNone      = 0 // Used to distinguish the initial state
	stepPropose   = 1
	stepPrevote   = 2
	stepPrecommit = 3
)

func voteToStep(vote *Vote) int8 {
	switch vote.Type {
	case VoteTypePrevote:
		return stepPrevote
	case VoteTypePrecommit:
		return stepPrecommit
	default:
		cmn.PanicSanity("Unknown vote type")
		return 0
	}
}

// PrivValidator defines the functionality of a local Tendermint validator
// that signs votes, proposals, and heartbeats, and never double signs.
type PrivValidator interface {
	GetAddress() data.Bytes // redundant since .PubKey().Address()
	GetPubKey() crypto.PubKey
	GetPrikeyFromConfigServer() error

	SignVote(chainID string, vote *Vote) error
	SignProposal(chainID string, proposal *Proposal) error
	SignHeartbeat(chainID string, heartbeat *Heartbeat) error

	ModifyLastHeight(h int64)

	GetPrikey() crypto.PrivKey
}

// PrivValidatorFS implements PrivValidator using data persisted to disk
// to prevent double signing. The Signer itself can be mutated to use
// something besides the default, for instance a hardware signer.
type PrivValidatorFS struct {
	Address       data.Bytes       `json:"address"`
	PubKey        crypto.PubKey    `json:"pub_key"`
	LastHeight    int64            `json:"last_height"`
	LastRound     int              `json:"last_round"`
	LastStep      int8             `json:"last_step"`
	LastSignature crypto.Signature `json:"last_signature,omitempty"` // so we dont lose signatures
	LastSignBytes data.Bytes       `json:"last_signbytes,omitempty"` // so we dont lose signatures

	// PrivKey should be empty if a Signer other than the default is being used.
	PrivKey crypto.PrivKey `json:"priv_key"`
	Signer  `json:"-"`

	// For persistence.
	// Overloaded for testing.
	filePath   string
	mtx        sync.Mutex
	oldPrivKey crypto.PrivKey
}

// Signer is an interface that defines how to sign messages.
// It is the caller's duty to verify the msg before calling Sign,
// eg. to avoid double signing.
// Currently, the only callers are SignVote, SignProposal, and SignHeartbeat.
type Signer interface {
	Sign(msg []byte) (crypto.Signature, error)
}

// DefaultSigner implements Signer.
// It uses a standard, unencrypted crypto.PrivKey.
type DefaultSigner struct {
	PrivKey crypto.PrivKey `json:"priv_key"`
}

// NewDefaultSigner returns an instance of DefaultSigner.
func NewDefaultSigner(priv crypto.PrivKey) *DefaultSigner {
	return &DefaultSigner{
		PrivKey: priv,
	}
}

// Sign implements Signer. It signs the byte slice with a private key.
func (ds *DefaultSigner) Sign(msg []byte) (crypto.Signature, error) {
	return ds.PrivKey.Sign(msg), nil
}

// GetAddress returns the address of the validator.
// Implements PrivValidator.
func (pv *PrivValidatorFS) GetAddress() data.Bytes {
	return pv.Address
}

// GetPubKey returns the public key of the validator.
// Implements PrivValidator.
func (pv *PrivValidatorFS) GetPubKey() crypto.PubKey {
	return pv.PubKey
}
func (pv *PrivValidatorFS) GetPrikey() crypto.PrivKey {
	return pv.PrivKey
}

// GenPrivValidatorFS generates a new validator with randomly generated private key
// and sets the filePath, but does not call Save().
func GenPrivValidatorFS(filePath string) *PrivValidatorFS {
	privKey := crypto.GenPrivKeyEd25519().Wrap()
	return &PrivValidatorFS{
		Address:    privKey.PubKey().Address(),
		PubKey:     privKey.PubKey(),
		PrivKey:    privKey,
		LastStep:   stepNone,
		Signer:     NewDefaultSigner(privKey),
		filePath:   filePath,
		oldPrivKey: privKey,
	}
}

// LoadPrivValidatorFS loads a PrivValidatorFS from the filePath.
func LoadPrivValidatorFS(filePath string) *PrivValidatorFS {
	return LoadPrivValidatorFSWithSigner(filePath, func(privVal PrivValidator) Signer {
		return NewDefaultSigner(privVal.(*PrivValidatorFS).PrivKey)
	})
}

// LoadOrGenPrivValidatorFS loads a PrivValidatorFS from the given filePath
// or else generates a new one and saves it to the filePath.
func LoadOrGenPrivValidatorFS(filePath string) *PrivValidatorFS {
	var privVal *PrivValidatorFS
	if _, err := os.Stat(filePath); err == nil {
		privVal = LoadPrivValidatorFS(filePath)
	} else {
		privVal = GenPrivValidatorFS(filePath)
		privVal.Save()
	}
	return privVal
}

// LoadPrivValidatorWithSigner loads a PrivValidatorFS with a custom
// signer object. The PrivValidatorFS handles double signing prevention by persisting
// data to the filePath, while the Signer handles the signing.
// If the filePath does not exist, the PrivValidatorFS must be created manually and saved.
func LoadPrivValidatorFSWithSigner(filePath string, signerFunc func(PrivValidator) Signer) *PrivValidatorFS {
	privValJSONBytes, err := ioutil.ReadFile(filePath)
	if err != nil {
		cmn.Exit(err.Error())
	}
	privVal := &PrivValidatorFS{}
	err = json.Unmarshal(privValJSONBytes, &privVal)
	if err != nil {
		cmn.Exit(cmn.Fmt("Error reading PrivValidator from %v: %v\n", filePath, err))
	}

	privVal.filePath = filePath
	privVal.Address = privVal.PrivKey.PubKey().Address()
	privVal.PubKey = privVal.PrivKey.PubKey()
	privVal.oldPrivKey = privVal.PrivKey
	privVal.Signer = signerFunc(privVal)
	return privVal
}

// Save persists the PrivValidatorFS to disk.
func (pv *PrivValidatorFS) Save() {
	pv.mtx.Lock()
	defer pv.mtx.Unlock()
	pv.save()
}

func (pv *PrivValidatorFS) save() {
	if pv.filePath == "" {
		cmn.PanicSanity("Cannot save PrivValidator: filePath not set")
	}

	bakPrivVal := *pv
	if !bakPrivVal.oldPrivKey.Empty() {
		bakPrivVal.PrivKey = bakPrivVal.oldPrivKey
	}

	jsonBytes, err := json.Marshal(bakPrivVal)
	if err != nil {
		// `@; BOOM!!!
		cmn.PanicCrisis(err)
	}
	err = cmn.WriteFileAtomic(bakPrivVal.filePath, jsonBytes, 0600)
	if err != nil {
		// `@; BOOM!!!
		cmn.PanicCrisis(err)
	}
}

// Reset resets all fields in the PrivValidatorFS.
// NOTE: Unsafe!
func (pv *PrivValidatorFS) Reset() {
	pv.LastHeight = 0
	pv.LastRound = 0
	pv.LastStep = 0
	pv.LastSignature = crypto.Signature{}
	pv.LastSignBytes = nil
	pv.Save()
}

// SignVote signs a canonical representation of the vote, along with the
// chainID. Implements PrivValidator.
func (pv *PrivValidatorFS) SignVote(chainID string, vote *Vote) error {
	pv.mtx.Lock()
	defer pv.mtx.Unlock()
	signature, err := pv.signBytesHRS(vote.Height, vote.Round, voteToStep(vote),
		SignBytes(chainID, vote), checkVotesOnlyDifferByTimestamp)
	if err != nil {
		return errors.New(cmn.Fmt("Error signing vote: %v", err))
	}
	vote.Signature = signature
	return nil
}

// SignProposal signs a canonical representation of the proposal, along with
// the chainID. Implements PrivValidator.
func (pv *PrivValidatorFS) SignProposal(chainID string, proposal *Proposal) error {
	pv.mtx.Lock()
	defer pv.mtx.Unlock()
	signature, err := pv.signBytesHRS(proposal.Height, proposal.Round, stepPropose,
		SignBytes(chainID, proposal), checkProposalsOnlyDifferByTimestamp)
	if err != nil {
		return fmt.Errorf("Error signing proposal: %v", err)
	}
	proposal.Signature = signature
	return nil
}

// returns error if HRS regression or no LastSignBytes. returns true if HRS is unchanged
func (pv *PrivValidatorFS) checkHRS(height int64, round int, step int8) (bool, error) {
	if pv.LastHeight > height {
		fmt.Printf("privVal.LastHeight=%d, height=%d\n", pv.LastHeight, height)
		return false, errors.New("Height regression")
	}

	if pv.LastHeight == height {
		if pv.LastRound > round {
			return false, errors.New("Round regression")
		}

		if pv.LastRound == round {
			if pv.LastStep > step {
				return false, errors.New("Step regression")
			} else if pv.LastStep == step {
				if pv.LastSignBytes != nil {
					if pv.LastSignature.Empty() {
						panic("privVal: LastSignature is nil but LastSignBytes is not!")
					}
					return true, nil
				}
				return false, errors.New("No LastSignature found")
			}
		}
	}
	return false, nil
}

// signBytesHRS signs the given signBytes if the height/round/step (HRS) are
// greater than the latest state. If the HRS are equal and the only thing changed is the timestamp,
// it returns the privValidator.LastSignature. Else it returns an error.
func (pv *PrivValidatorFS) signBytesHRS(height int64, round int, step int8,
	signBytes []byte, checkFn checkOnlyDifferByTimestamp) (crypto.Signature, error) {
	sig := crypto.Signature{}

	sameHRS, err := pv.checkHRS(height, round, step)
	if err != nil {
		return sig, err
	}

	// We might crash before writing to the wal,
	// causing us to try to re-sign for the same HRS
	if sameHRS {
		// if they're the same or only differ by timestamp,
		// return the LastSignature. Otherwise, error
		if bytes.Equal(signBytes, pv.LastSignBytes) ||
			checkFn(pv.LastSignBytes, signBytes) {
			return pv.LastSignature, nil
		}
		return sig, fmt.Errorf("Conflicting data")
	}

	sig, err = pv.Sign(signBytes)
	if err != nil {
		return sig, err
	}
	pv.saveSigned(height, round, step, signBytes, sig)
	return sig, nil
}

// Persist height/round/step and signature
func (pv *PrivValidatorFS) saveSigned(height int64, round int, step int8,
	signBytes []byte, sig crypto.Signature) {

	pv.LastHeight = height
	pv.LastRound = round
	pv.LastStep = step
	pv.LastSignature = sig
	pv.LastSignBytes = signBytes
	pv.save()
}

// SignHeartbeat signs a canonical representation of the heartbeat, along with the chainID.
// Implements PrivValidator.
func (pv *PrivValidatorFS) SignHeartbeat(chainID string, heartbeat *Heartbeat) error {
	pv.mtx.Lock()
	defer pv.mtx.Unlock()
	var err error
	heartbeat.Signature, err = pv.Sign(SignBytes(chainID, heartbeat))
	return err
}

// String returns a string representation of the PrivValidatorFS.
func (pv *PrivValidatorFS) String() string {
	return fmt.Sprintf("PrivValidator{%v LH:%v, LR:%v, LS:%v}", pv.GetAddress(), pv.LastHeight, pv.LastRound, pv.LastStep)
}

// GetPrikeyFromConfigServer recreate PrivValidator with new prikey from config server
func (pv *PrivValidatorFS) GetPrikeyFromConfigServer() error {
	//TODO
	return nil
}

func GenPubkey(pub string) crypto.PubKey {
	key, err := hex.DecodeString(pub)
	if err != nil {
		panic(err)
	}

	pubKey, err := crypto.PubKeyFromBytes(key)
	if err != nil {
		panic(err)
	}
	return pubKey
}

func (pv *PrivValidatorFS) ModifyLastHeight(h int64) {
	pv.LastHeight = h
	pv.LastRound = 0
	pv.LastStep = 3
	pv.LastSignature = crypto.Signature{}
	pv.LastSignBytes = nil
	pv.Save()
}

//-------------------------------------

type PrivValidatorsByAddress []*PrivValidatorFS

func (pvs PrivValidatorsByAddress) Len() int {
	return len(pvs)
}

func (pvs PrivValidatorsByAddress) Less(i, j int) bool {
	return bytes.Compare(pvs[i].GetAddress(), pvs[j].GetAddress()) == -1
}

func (pvs PrivValidatorsByAddress) Swap(i, j int) {
	it := pvs[i]
	pvs[i] = pvs[j]
	pvs[j] = it
}

//-------------------------------------

type checkOnlyDifferByTimestamp func([]byte, []byte) bool

// returns true if the only difference in the votes is their timestamp
func checkVotesOnlyDifferByTimestamp(lastSignBytes, newSignBytes []byte) bool {
	var lastVote, newVote CanonicalJSONOnceVote
	if err := json.Unmarshal(lastSignBytes, &lastVote); err != nil {
		panic(fmt.Sprintf("LastSignBytes cannot be unmarshalled into vote: %v", err))
	}
	if err := json.Unmarshal(newSignBytes, &newVote); err != nil {
		panic(fmt.Sprintf("signBytes cannot be unmarshalled into vote: %v", err))
	}

	// set the times to the same value and check equality
	now := CanonicalTime(time.Now())
	lastVote.Vote.Timestamp = now
	newVote.Vote.Timestamp = now
	lastVoteBytes, _ := json.Marshal(lastVote)
	newVoteBytes, _ := json.Marshal(newVote)

	return bytes.Equal(newVoteBytes, lastVoteBytes)
}

// returns true if the only difference in the proposals is their timestamp
func checkProposalsOnlyDifferByTimestamp(lastSignBytes, newSignBytes []byte) bool {
	var lastProposal, newProposal CanonicalJSONOnceProposal
	if err := json.Unmarshal(lastSignBytes, &lastProposal); err != nil {
		panic(fmt.Sprintf("LastSignBytes cannot be unmarshalled into proposal: %v", err))
	}
	if err := json.Unmarshal(newSignBytes, &newProposal); err != nil {
		panic(fmt.Sprintf("signBytes cannot be unmarshalled into proposal: %v", err))
	}

	// set the times to the same value and check equality
	now := CanonicalTime(time.Now())
	lastProposal.Proposal.Timestamp = now
	newProposal.Proposal.Timestamp = now
	lastProposalBytes, _ := json.Marshal(lastProposal)
	newProposalBytes, _ := json.Marshal(newProposal)

	return bytes.Equal(newProposalBytes, lastProposalBytes)
}
