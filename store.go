// distkv provides a simple and secure distributed key-value store. The keys and
// associated values are changed via distributed consensus over an authenticated
// ssh channel. This means that the values are changed only when a majority of nodes
// in the cluster agree on the new value.
//
// Distributed consensus is provided via the Raft algorithm.
package distkv

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"errors"
	"github.com/hashicorp/raft"
	"github.com/hashicorp/raft-boltdb"
	"golang.org/x/crypto/ssh"
	"net"
)

const (
	retainSnapshotCount = 2
	raftTimeout         = 10 * time.Second
)

var (
	noAuthorizedPeers  = errors.New("No authorized peers file")
	ShutdownError      = errors.New("Store was shutdown")
	AlreadyOpenedError = errors.New("Store was already opened")
)

type command struct {
	Op    string
	Key   string
	Value []byte
}

// Store is a simple key-value store, where all changes are made via Raft consensus.
type Store struct {
	RaftDir          string
	RaftBind         string
	authMethodPubKey ssh.AuthMethod
	checkHostKey     func(addr string, remote net.Addr, key ssh.PublicKey) error
	sshTransport     *sshTransport
	raftTransport    *raft.NetworkTransport

	mu     sync.Mutex
	m      map[string][]byte // The key-value store for the system.
	opened bool

	raft *raft.Raft // The consensus mechanism

	logger *log.Logger
}

// New returns a new Store.
// If debug is true, informational and debug messages are printed to os.Stderr
func NewStore(debug bool) *Store {

	logSink := ioutil.Discard

	if debug {
		logSink = os.Stderr
	}

	return &Store{
		m:      make(map[string][]byte),
		logger: log.New(logSink, "[distkv] ", log.LstdFlags),
	}
}

// Open opens the store. If enableSingle is set, and there are no existing peers,
// then this node becomes the first node, and therefore leader, of the cluster.
func (s *Store) Open(enableSingle bool) error {

	if s.opened {
		return AlreadyOpenedError
	}
	s.opened = true

	// Setup Raft configuration.
	config := raft.DefaultConfig()
	config.Logger = s.logger

	// Check for any existing peers.
	peers, err := readPeersJSON(filepath.Join(s.RaftDir, "peers.json"))
	if err != nil {
		return err
	}

	// Allow the node to entry single-mode, potentially electing itself, if
	// explicitly enabled and there is only 1 node in the cluster already.
	if enableSingle && len(peers) <= 1 {
		s.logger.Println("enabling single-node mode")
		config.EnableSingleNode = true
		config.DisableBootstrapAfterElect = false
	}

	//TODO add error return to newSSHTransport
	s.sshTransport, s.raftTransport, err = newSSHTransport(s.RaftBind, s.RaftDir, s.logger)

	if err != nil {
		s.logger.Println("Error initializing ssh transport:", err)
		return err
	}

	s.authMethodPubKey = ssh.PublicKeys(s.sshTransport.privateKey)
	s.checkHostKey = s.sshTransport.checkHostKey

	// Create peer storage.
	peerStore := raft.NewJSONPeers(s.RaftDir, s.raftTransport)

	// Create the snapshot store. This allows the Raft to truncate the log.
	snapshots, err := raft.NewFileSnapshotStore(s.RaftDir, retainSnapshotCount, os.Stderr)
	if err != nil {
		return fmt.Errorf("file snapshot store: %s", err)
	}

	// Create the log store and stable store.
	logStore, err := raftboltdb.NewBoltStore(filepath.Join(s.RaftDir, "raft.db"))
	if err != nil {
		return fmt.Errorf("new bolt store: %s", err)
	}

	// Instantiate the Raft systems.
	ra, err := raft.NewRaft(config, (*fsm)(s), logStore, logStore, snapshots, peerStore, s.raftTransport)
	if err != nil {
		return fmt.Errorf("new raft: %s", err)
	}
	s.raft = ra

	go func() {

		for {
			joinMessage, notClosed := <-s.sshTransport.joinMessage

			if !notClosed {
				return
			}

			if s.raft.State() != raft.Leader {
				//TODO Forward to current leader?
				s.logger.Println("No leader but received join request. Ignoring:", joinMessage)
				joinMessage.returnChan <- false
				close(joinMessage.returnChan)
				continue
			}

			err := s.join(joinMessage.joinAddr)

			if err != nil {
				s.logger.Println("Error during join request:", err)
				joinMessage.returnChan <- false
			} else {
				joinMessage.returnChan <- true
			}

			close(joinMessage.returnChan)
		}

	}()

	go func() {

		for {
			leaderMessage, notClosed := <-s.sshTransport.leaderMessage

			if !notClosed {
				return
			}

			if s.raft.State() != raft.Leader {
				//TODO Forward to current leader?
				s.logger.Println("No leader but received leader request. Ignoring:", *leaderMessage.cmd)
				leaderMessage.returnChan <- false
				close(leaderMessage.returnChan)
				continue
			}

			c := leaderMessage.cmd
			sc, err := serializeCommand(c)
			if err != nil {
				s.logger.Println("Error serializing command in leader request:", *leaderMessage.cmd)
				leaderMessage.returnChan <- false
				close(leaderMessage.returnChan)
				continue
			}

			f := s.raft.Apply(sc, raftTimeout)
			if err, ok := f.(error); ok {
				s.logger.Println("Error applying command in leader request:", *leaderMessage.cmd, err)
				leaderMessage.returnChan <- false
				close(leaderMessage.returnChan)
				continue
			}

			if f.Error() != nil {
				s.logger.Println("Error distributing command in leader request:", *leaderMessage.cmd, err)
				leaderMessage.returnChan <- false
				close(leaderMessage.returnChan)
				continue
			}

			leaderMessage.returnChan <- true
			close(leaderMessage.returnChan)
		}

	}()

	return nil
}

// Close closes the store after stepping down as node/leader.
func (s *Store) Close() error {

	shutdownFuture := s.raft.Shutdown()

	if err := shutdownFuture.Error(); err != nil {
		s.logger.Println("raft shutdown error:", err)
		return err
	}

	if err := s.raftTransport.Close(); err != nil {
		s.logger.Println("raft transport close error:", err)
		return err
	}

	s.logger.Println("successfully shutdown")
	return nil

}

// Join joins a node reachable under raftAddr, to the cluster lead by the
// node reachable under joinAddr. The joined node must be ready to respond to Raft
// communications at that raftAddr.
func (s *Store) Join(joinAddr, raftAddr string) error {

	if err := s.checkState(); err != nil {
		return err
	}

	sshClientConfig := &ssh.ClientConfig{
		User: protocolUser,
		Auth: []ssh.AuthMethod{
			s.authMethodPubKey,
		},
		HostKeyCallback: s.checkHostKey,
	}

	serverConn, err := ssh.Dial("tcp", joinAddr, sshClientConfig)
	if err != nil {
		s.logger.Printf("Server dial error: %s\n", err)
		return err
	}

	reply, _, err := serverConn.SendRequest(joinRequestType, true, []byte(raftAddr))

	if err != nil {
		s.logger.Println("Error sending out-of-band join request:", err)
		return err
	}

	if reply != true {
		s.logger.Printf("Error adding peer on join node %s: %s\n", joinAddr, err)
		return err
	}

	return nil

}

func (s *Store) checkState() error {

	if s.raft.State() == raft.Shutdown {
		return ShutdownError
	}

	return nil

}

func (s *Store) leaderRequest(op *command) error {

	if err := s.checkState(); err != nil {
		return err
	}

	sshClientConfig := &ssh.ClientConfig{
		User: protocolUser,
		Auth: []ssh.AuthMethod{
			s.authMethodPubKey,
		},
		HostKeyCallback: s.checkHostKey,
	}

	serverConn, err := ssh.Dial("tcp", s.raft.Leader(), sshClientConfig)
	if err != nil {
		s.logger.Printf("Server dial error: %s\n", err)
		return err
	}

	sc, err := serializeCommand(op)
	if err != nil {
		s.logger.Printf("Command serialization error: %s\n", err)
		return err
	}

	reply, _, err := serverConn.SendRequest(leaderMessageType, true, sc)

	if err != nil {
		s.logger.Println("Error sending out-of-band leader request:", err)
		return err
	}

	if reply != true {
		s.logger.Printf("Error executing command on leader node %s: %s\n", s.raft.Leader(), err)
		return err
	}

	return nil

}

// Get returns the value for the given key.
// TODO implement strongly consistent read with extra argument or func
func (s *Store) Get(key string) ([]byte, error) {
	if err := s.checkState(); err != nil {
		return []byte{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[key], nil
}

// Set sets the value for the given key.
func (s *Store) Set(key string, value []byte) error {
	if err := s.checkState(); err != nil {
		return err
	}

	c := &command{
		Op:    "set",
		Key:   key,
		Value: value,
	}

	if s.raft.State() != raft.Leader {
		s.logger.Println("Forwarding Set command to leader: ", s.raft.Leader())
		return s.leaderRequest(c)
	}

	sc, err := serializeCommand(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(sc, raftTimeout)
	if err, ok := f.(error); ok {
		return err
	}

	return f.Error()
}

// Delete deletes the given key.
func (s *Store) Delete(key string) error {
	if err := s.checkState(); err != nil {
		return err
	}

	c := &command{
		Op:  "delete",
		Key: key,
	}

	if s.raft.State() != raft.Leader {
		s.logger.Println("Forwarding Delete command to leader: ", s.raft.Leader())

		return s.leaderRequest(c)
	}

	sc, err := serializeCommand(c)
	if err != nil {
		return err
	}

	f := s.raft.Apply(sc, raftTimeout)
	if err, ok := f.(error); ok {
		return err
	}

	return f.Error()
}

func (s *Store) join(addr string) error {
	s.logger.Printf("received join request for remote node as %s", addr)

	f := s.raft.AddPeer(addr)
	if f.Error() != nil {
		return f.Error()
	}
	s.logger.Printf("node at %s joined successfully", addr)
	return nil
}

type fsm Store

// Apply applies a Raft log entry to the key-value store.
func (f *fsm) Apply(l *raft.Log) interface{} {
	dsc, err := deserializeCommand(l.Data)
	fatalLogger := log.New(os.Stderr, "[distkv]", log.LstdFlags)

	if err != nil {
		//TODO fix fatal for library
		fatalLogger.Fatalf("error in deserializeCommand: %s\n", err)
	}

	switch dsc.Op {
	case "set":
		return f.applySet(dsc.Key, dsc.Value)
	case "delete":
		return f.applyDelete(dsc.Key)
	default:
		//TODO fix fatal for library
		fatalLogger.Fatalf("unrecognized command op: %s", dsc.Op)
		return nil
	}
}

// Snapshot returns a snapshot of the key-value store.
func (f *fsm) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	// Clone the map.
	o := make(map[string][]byte)
	for k, v := range f.m {
		o[k] = v
	}
	return &fsmSnapshot{store: o}, nil
}

// Restore stores the key-value store to a previous state.
func (f *fsm) Restore(rc io.ReadCloser) error {
	o := make(map[string][]byte)

	decoder := gob.NewDecoder(rc)
	err := decoder.Decode(&o)

	if err != nil {
		return err
	}

	// Set the state from the snapshot, no lock required according to
	// Hashicorp docs.
	f.m = o
	return nil
}

func (f *fsm) applySet(key string, value []byte) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = value
	return nil
}

func (f *fsm) applyDelete(key string) interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, key)
	return nil
}

type fsmSnapshot struct {
	store map[string][]byte
}

func (f *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	err := func() error {
		// Encode data.

		buf := bytes.NewBuffer([]byte{})

		encoder := gob.NewEncoder(buf)
		err := encoder.Encode(f.store)

		if err != nil {
			return err
		}

		var n int
		// Write data to sink.
		if n, err = sink.Write(buf.Bytes()); err != nil {
			return err
		}

		if n != buf.Len() {
			return fmt.Errorf("Incomplete write for snapshot")
		}

		// Close the sink.
		if err := sink.Close(); err != nil {
			return err
		}

		return nil
	}()

	if err != nil {
		sink.Cancel()
		return err
	}

	return nil
}

func (f *fsmSnapshot) Release() {
	//TODO snapshot release function
}

func readPeersJSON(path string) ([]string, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if len(b) == 0 {
		return nil, nil
	}

	var peers []string
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&peers); err != nil {
		return nil, err
	}

	return peers, nil
}

func serializeCommand(c *command) ([]byte, error) {

	buf := bytes.NewBuffer([]byte{})

	encoder := gob.NewEncoder(buf)
	err := encoder.Encode(c)

	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil

}

func deserializeCommand(sc []byte) (*command, error) {

	if len(sc) < 1 {
		return nil, fmt.Errorf("Zero length serialization passed")
	}

	buf := bytes.NewBuffer(sc)

	decoder := gob.NewDecoder(buf)

	command := &command{}

	err := decoder.Decode(command)

	if err != nil {
		return nil, err
	}

	return command, nil

}
