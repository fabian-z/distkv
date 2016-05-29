package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
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
	raftDir      string
	peerPubkeys  peerPublicKeys
	listener     *streamSSHLayer
	serverConfig *ssh.ServerConfig
	config       *ssh.ClientConfig
}

type peerPublicKeys struct {
	sync.RWMutex
	pubkeys map[string]ssh.PublicKey
}

const bogusAddress string = "127.0.0.1:0"

const joinRequestType string = "joinRequest"

type joinMessage struct {
	joinAddr   string
	returnChan chan bool
}

func newSSHTransport(bindAddr string, raftDir string) (chan joinMessage, ssh.Signer, *raft.NetworkTransport) {

	s := new(sshTransport)
	s.raftDir = raftDir

	//TODO load peerPubkeys from json

	// An SSH server is represented by a ServerConfig, which holds
	// certificate details and handles authentication of ServerConns.
	config := &ssh.ServerConfig{
		PublicKeyCallback: s.keyAuth,
	}

	privateBytes, err := ioutil.ReadFile(filepath.Join(s.raftDir, "id_rsa"))
	if err != nil {
		log.Println("Failed to load private key, trying to generate a new pair")
		privateBytes = s.generateSSHKey()
	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		log.Fatal("Failed to parse private key:", err)
	}

	pubBytes := ssh.Marshal(private.PublicKey())

	log.Println("Node public key is: ", private.PublicKey().Type(), base64.StdEncoding.EncodeToString(pubBytes))

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

	s.listener = &streamSSHLayer{
		sshListener:  listener,
		incoming:     make(chan sshConn, 15),
		clientConfig: sshClientConfig,
	}

	joinChan := make(chan joinMessage)

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
				go handleRequests(joinChan, reqs)

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

					s.listener.incoming <- sshConn{
						channel,
						sshConnection.LocalAddr(),
						sshConnection.RemoteAddr(),
					}

				}
			}()

		}

	}()

	return joinChan, private, raft.NewNetworkTransport(s.listener, 5, 10*time.Second, nil)

}

func handleRequests(joinChannel chan joinMessage, reqs <-chan *ssh.Request) {

	for req := range reqs {
		if req.Type == joinRequestType {
			log.Printf("Received out-of-band request: %+v", req)

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
			}

		}
	}
}

func (transport *sshTransport) keyAuth(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {

	log.Println(conn.RemoteAddr(), "authenticate with", key.Type(), "for user", conn.User())
	log.Println(base64.StdEncoding.EncodeToString(key.Marshal()))

	//TODO check public key against transport.peerPubkeys
	//Endpoint IP
	//key.Type() + " " + base64.StdEncoding.EncodeToString(key.Marshal())
	return nil, nil
}

func (transport *sshTransport) generateSSHKey() (privateKeyPem []byte) {

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

	//persist keypair to raftDir
	err = ioutil.WriteFile(filepath.Join(transport.raftDir, "id_rsa"), privateKeyPem, 0600)

	if err != nil {
		log.Fatal("Error persisting generated ssh private key:", err)
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
