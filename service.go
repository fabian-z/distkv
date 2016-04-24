// service.go provides the RPC cluster communication for accessing the distributed key-value store.
// It also provides the endpoint for other nodes to join an existing cluster.
package main

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"net/rpc"
)

// Store is the interface Raft-backed key-value stores must implement.
type StoreInterface interface {
	// Get returns the value for the given key.
	Get(key string) ([]byte, error)

	// Set sets the value for the given key, via distributed consensus.
	Set(key string, value []byte) error

	// Delete removes the given key, via distributed consensus.
	Delete(key string) error

	// Join joins the node, reachable at addr, to the cluster.
	Join(addr string) error
}

// Service provides RPC service.
type Service struct {
	addr string
	ln   net.Listener

	store StoreInterface
}

//TODO support []KeyValue in Set
type KeyValue struct {
	Key   string
	Value []byte
}

// New returns an uninitialized HTTP service.
func NewRPC(addr string, store StoreInterface) *Service {
	return &Service{
		addr:  addr,
		store: store,
	}
}

// Start starts the service.
func (s *Service) start() error {

	rpc.Register(s)
	rpc.HandleHTTP()
	ln, err := net.Listen("tcp", s.addr)

	if err != nil {
		log.Printf("rpc listen error (%s) for addr: %s", err, s.addr)
		return err
	}

	s.ln = ln

	go log.Println(http.Serve(ln, nil))

	return nil
}

// Close closes the service.
func (s *Service) close() {
	s.ln.Close()
	return
}

// Join provides an RPC call that allows to register a new node to the cluster
func (s *Service) Join(remoteAddr string, reply *bool) error {
	*reply = false
	if len(remoteAddr) < 2 {
		return fmt.Errorf("Invalid address")
	}

	if err := s.store.Join(remoteAddr); err != nil {
		return err
	}

	*reply = true
	return nil
}

func (s *Service) Get(key string, reply *[]byte) error {

	if len(key) == 0 {
		return fmt.Errorf("Empty key passed")
	}

	value, err := s.store.Get(key)

	if err != nil {
		return err
	}

	*reply = value
	return nil

}

func (s *Service) Set(kv KeyValue, reply *bool) error {

	*reply = false

	if len(kv.Key) == 0 {
		return fmt.Errorf("Empty key passed")
	}

	if err := s.store.Set(kv.Key, kv.Value); err != nil {
		return err
	}

	*reply = true
	return nil

}

func (s *Service) Delete(key string, reply *bool) error {

	*reply = false

	if len(key) == 0 {
		return fmt.Errorf("Empty key passed")
	}

	if err := s.store.Delete(key); err != nil {
		return err
	}

	//Second delete really needed?
	//s.store.Delete(key)

	*reply = true
	return nil

}
