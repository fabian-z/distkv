package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"golang.org/x/crypto/ssh"
)

// Command line defaults
const (
	DefaultRaftAddr = ":12000"
)

// Command line parameters
var rpcAddr string
var raftAddr string
var joinAddr string

func init() {
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

	s := NewStore()
	s.RaftDir = raftDir
	s.RaftBind = raftAddr
	if err := s.Open(joinAddr == ""); err != nil {
		log.Fatalf("failed to open store: %s", err.Error())
	}

	// If join was specified, make the join request.
	if joinAddr != "" {
		if err := join(joinAddr, raftAddr, s.privateKey); err != nil {
			log.Fatalf("failed to join node at %s: %s", joinAddr, err.Error())
		}
	}

	log.Println("hraft started successfully")

	// Block forever.
	select {}
}

func join(joinAddr, raftAddr string, privateKey ssh.Signer) error {

	sshClientConfig := &ssh.ClientConfig{
		User: "raft",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(privateKey),
		},
	}

	serverConn, err := ssh.Dial("tcp", joinAddr, sshClientConfig)
	if err != nil {
		log.Printf("Server dial error: %s\n", err)
		return err
	}

	reply, _, err := serverConn.SendRequest(joinRequestType, true, []byte(raftAddr))

	if err != nil {
		log.Println("Error sending out-of-band join request:", err)
		return err
	}

	if reply != true {
		log.Printf("Error adding peer on join node %s: %s\n", err)
		return err
	}

	return nil

}
