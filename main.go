package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/otoolep/hraftd/store"
	"net/rpc"
)

// Command line defaults
const (
	DefaultRPCAddr  = ":11000"
	DefaultRaftAddr = ":12000"
)

// Command line parameters
var rpcAddr string
var raftAddr string
var joinAddr string

func init() {
	flag.StringVar(&rpcAddr, "haddr", DefaultRPCAddr, "Set the RPC bind address")
	flag.StringVar(&raftAddr, "raddr", DefaultRaftAddr, "Set Raft bind address")
	flag.StringVar(&joinAddr, "join", "", "Set join address, if any")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options] <raft-data-path> \n", os.Args[0])
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()

	if flag.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "No Raft storage directory specified\n")
		os.Exit(1)
	}

	// Ensure Raft storage exists.
	raftDir := flag.Arg(0)
	if raftDir == "" {
		fmt.Fprintf(os.Stderr, "No Raft storage directory specified\n")
		os.Exit(1)
	}
	os.MkdirAll(raftDir, 0700)

	s := store.New()
	s.RaftDir = raftDir
	s.RaftBind = raftAddr
	if err := s.Open(joinAddr == ""); err != nil {
		log.Fatalf("failed to open store: %s", err.Error())
	}

	rpcInstance := NewRPC(rpcAddr, s)
	if err := rpcInstance.start(); err != nil {
		log.Fatalf("failed to start RPC service: %s", err.Error())
	}

	// If join was specified, make the join request.
	if joinAddr != "" {
		if err := join(joinAddr, raftAddr); err != nil {
			log.Fatalf("failed to join node at %s: %s", joinAddr, err.Error())
		}
	}

	log.Println("hraft started successfully")

	// Block forever.
	select {}
}

func join(joinAddr, raftAddr string) error {

	client, err := rpc.DialHTTP("tcp", joinAddr)

	if err != nil {
		log.Printf("error (%s) dialing %s:\n", err, joinAddr)
		return err
	}

	// Synchronous call
	var reply bool
	err = client.Call("Service.Join", raftAddr, &reply)

	if err != nil {
		log.Printf("error invoking join %s:\n", err)
		return err
	}

	if !reply {
		unknownErr := fmt.Errorf("unknown error in rpc join call")
		log.Println(unknownErr)
		return unknownErr
	}

	return nil

}
