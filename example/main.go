package main

import (
	"flag"
	"fmt"
	"github.com/fabian-z/distkv"
	"log"
	"os"
	"path/filepath"
)

// Command line defaults
const (
	DefaultRaftAddr = ":12000"
)

// Command line parameters
var raftAddr string
var joinAddr string
var raftDir string

func init() {

	workingDirectory, err := os.Getwd()

	if err != nil {
		log.Fatal("Error getting working directory:", err)
	}

	defaultRaftDir := filepath.Join(workingDirectory, "/raft")

	flag.StringVar(&raftAddr, "raftsocket", DefaultRaftAddr, "Set Raft bind address")
	flag.StringVar(&joinAddr, "raftjoin", "", "Set join address, if any")
	flag.StringVar(&raftDir, "raftdir", defaultRaftDir, "Set raft directory")
}

func main() {

	if !flag.Parsed() {
		flag.Parse()
	}

	// Ensure Raft storage exists.
	if raftDir == "" {
		fmt.Fprintf(os.Stderr, "No Raft storage directory specified\n")
		os.Exit(1)
	}

	os.MkdirAll(raftDir, 0700)

	s := distkv.NewStore()
	s.RaftDir = raftDir
	s.RaftBind = raftAddr
	if err := s.Open(joinAddr == ""); err != nil {
		log.Fatalf("failed to open store: %s", err.Error())
	}

	// If join was specified, make the join request.
	if joinAddr != "" {
		if err := s.Join(joinAddr, raftAddr); err != nil {
			log.Fatalf("failed to join node at %s: %s", joinAddr, err.Error())
		}
	}

	log.Println("distkv started successfully")

	// Block forever.
	select {}
}
