package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"github.com/hashicorp/raft"
	"golang.org/x/crypto/ssh"
	"io/ioutil"
	"log"
	"net"
	"path/filepath"
	"sync"
	"time"
)

type sshTransport struct {
	peerPubkeys   *peerPublicKeys
	joinMessage   chan joinMessage
	leaderMessage chan leaderMessage
	privateKey    ssh.Signer
}

type peerPublicKeys struct {
	sync.RWMutex
	pubkeys map[string]ssh.PublicKey
}

const bogusAddress string = "127.0.0.1:0"

const (
	maxPoolConnections        = 5
	connectionTimeout         = 10 * time.Second
	protocolUser       string = "raft"
	joinRequestType    string = "joinRequest"
	leaderMessageType  string = "leaderMessage"
)

type joinMessage struct {
	joinAddr   string
	returnChan chan bool
}

type leaderMessage struct {
	cmd        *command
	returnChan chan bool
}

func newSSHTransport(bindAddr string, raftDir string) (*sshTransport, *raft.NetworkTransport) {

	s := new(sshTransport)

	//TODO load peerPubkeys from json

	// An SSH server is represented by a ServerConfig, which holds
	// certificate details and handles authentication of ServerConns.
	config := &ssh.ServerConfig{
		PublicKeyCallback: s.keyAuth,
	}

	privateBytes, err := ioutil.ReadFile(filepath.Join(raftDir, "id_rsa"))
	if err != nil {
		log.Println("Failed to load private key, trying to generate a new pair")
		privateBytes = generateSSHKey(raftDir)
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		log.Fatal("Failed to parse private key:", err)
	}

	pubBytes := ssh.Marshal(private.PublicKey())

	log.Println("Node public key is: ", private.PublicKey().Type(), base64.StdEncoding.EncodeToString(pubBytes))

	s.privateKey = private
	config.AddHostKey(private)

	// Once a ServerConfig has been configured, connections can be
	// accepted.
	listener, err := net.Listen("tcp", bindAddr)

	if err != nil {
		log.Fatal("failed to listen for connection on", bindAddr, ":", err)
	}

	sshClientConfig := &ssh.ClientConfig{
		User: "raft",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(private),
		},
	}

	raftListener := &streamSSHLayer{
		sshListener:  listener,
		incoming:     make(chan sshConn, 15),
		clientConfig: sshClientConfig,
	}

	s.joinMessage = make(chan joinMessage)
	s.leaderMessage = make(chan leaderMessage)

	go func() {

		for {

			nConn, err := listener.Accept()

			if err != nil {
				//TODO improve failure path after closing listener
				log.Fatal("failed to accept incoming connection:", err)
			}

			go func() {

				// Before use, a handshake must be performed on the incoming
				// net.Conn.
				sshConnection, chans, reqs, err := ssh.NewServerConn(nConn, config)
				if err != nil {
					log.Println("Failed to handshake:", err)
					nConn.Close()
					return
				}
				// The incoming Request channel must be serviced.
				go handleRequests(s.joinMessage, s.leaderMessage, reqs)

				// Service the incoming Channel channel.
				for newChannel := range chans {

					if newChannel.ChannelType() != "direct-tcpip" {
						newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
						continue
					}

					channel, requests, err := newChannel.Accept()
					if err != nil {
						log.Println("Could not accept channel:", err)
						continue
					}

					go ssh.DiscardRequests(requests)

					raftListener.incoming <- sshConn{
						channel,
						sshConnection.LocalAddr(),
						sshConnection.RemoteAddr(),
					}

				}
			}()

		}

	}()

	return s, raft.NewNetworkTransport(raftListener, maxPoolConnections, connectionTimeout, nil)

}

func handleRequests(joinChannel chan joinMessage, leaderMessageChan chan leaderMessage, reqs <-chan *ssh.Request) {

	for req := range reqs {
		log.Printf("Received out-of-band request: %+v", req)
		if req.Type == joinRequestType {

			returnChan := make(chan bool)
			msg := joinMessage{joinAddr: string(req.Payload), returnChan: returnChan}
			joinChannel <- msg

			timeout := time.After(15 * time.Second)
			select {
			case response := <-returnChan:
				err := req.Reply(response, req.Payload)
				if err != nil {
					log.Println("Error replying to join request for:", string(req.Payload))
				}
			case <-timeout:
				log.Println("Timed out processing join request for:", string(req.Payload))
				err := req.Reply(false, []byte{})
				if err != nil {
					log.Println("Error replying to join request for:", string(req.Payload))
				}
			}

			continue

		}

		if req.Type == leaderMessageType {

			returnChan := make(chan bool)

			//Decode payload

			cmd, err := deserializeCommand(req.Payload)

			if err != nil {
				log.Println("Error deserializing payload:", err)
				err := req.Reply(false, []byte{})
				if err != nil {
					log.Println("Error replying to leader request for:", string(req.Payload))
				}
			}

			msg := leaderMessage{cmd: cmd, returnChan: returnChan}
			leaderMessageChan <- msg

			timeout := time.After(connectionTimeout)
			select {
			case response := <-returnChan:
				err := req.Reply(response, []byte{})
				if err != nil {
					log.Println("Error replying to leader request for:", cmd)
				}
			case <-timeout:
				log.Println("Timed out processing leader request for:", cmd)
				err := req.Reply(false, []byte{})
				if err != nil {
					log.Println("Error replying to leader request for:", cmd)
				}
			}

			continue

		}

		log.Printf("Did not handle out of band request: %+v", req)
	}
}

func (transport *sshTransport) keyAuth(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {

	log.Println(conn.RemoteAddr(), "authenticate with", key.Type(), "for user", conn.User())
	log.Println(base64.StdEncoding.EncodeToString(key.Marshal()))

	//TODO check public key against transport.peerPubkeys
	//Endpoint IP
	//key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())

	if conn.User() != "raft" {
		return nil, errors.New("Wrong user for protocol offered by server")
	}

	return nil, nil
}

func generateSSHKey(targetDir string) (privateKeyPem []byte) {

	//generate 4096 bit rsa keypair
	privateKey, err := rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		log.Fatal("Error generating private key:", err)
	}

	privateKeyDer := x509.MarshalPKCS1PrivateKey(privateKey)
	privateKeyBlock := pem.Block{
		Type:    "RSA PRIVATE KEY",
		Headers: nil,
		Bytes:   privateKeyDer,
	}

	privateKeyPem = pem.EncodeToMemory(&privateKeyBlock)

	if len(targetDir) > 0 {
		//persist key to raftDir
		err = ioutil.WriteFile(filepath.Join(targetDir, "id_rsa"), privateKeyPem, 0600)

		if err != nil {
			log.Fatal("Error persisting generated ssh private key:", err)
		}
	}

	return

}

type streamSSHLayer struct {
	sshListener  net.Listener
	incoming     chan sshConn
	clientConfig *ssh.ClientConfig
}

func (listener *streamSSHLayer) Accept() (net.Conn, error) {

	select {
	case l := <-listener.incoming:
		wrapper := &sshConn{l, l.localAddr, l.remoteAddr}
		return wrapper, nil
	}

}

func (listener *streamSSHLayer) Close() error {
	return listener.sshListener.Close()
}

func (listener *streamSSHLayer) Addr() net.Addr {
	return listener.sshListener.Addr()
}

func (listener *streamSSHLayer) Dial(address string, timeout time.Duration) (net.Conn, error) {

	serverConn, err := ssh.Dial("tcp", address, listener.clientConfig)
	if err != nil {
		log.Printf("Server dial error: %s\n", err)
		return nil, err
	}

	//client address given here is bogus and ignored by server
	remoteConn, err := serverConn.Dial("tcp", bogusAddress)
	if err != nil {
		log.Printf("Remote dial error: %s\n", err)
		return nil, err
	}

	return remoteConn, nil

}

type sshConn struct {
	ssh.Channel
	localAddr  net.Addr
	remoteAddr net.Addr
}

func (wrapper *sshConn) LocalAddr() net.Addr {
	return wrapper.localAddr
}

func (wrapper *sshConn) RemoteAddr() net.Addr {
	return wrapper.remoteAddr
}

//TODO IO timeout operations support
func (wrapper *sshConn) SetDeadline(t time.Time) error {
	return nil
}

func (wrapper *sshConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (wrapper *sshConn) SetWriteDeadline(t time.Time) error {
	return nil
}
