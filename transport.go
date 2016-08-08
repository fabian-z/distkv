package distkv

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"github.com/hashicorp/raft"
	"golang.org/x/crypto/ssh"
	"io/ioutil"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type sshTransport struct {
	peerPubkeys   *peerPublicKeys
	joinMessage   chan joinMessage
	leaderMessage chan leaderMessage
	privateKey    ssh.Signer
	logger        *log.Logger
}

type peerPublicKeys struct {
	sync.RWMutex
	pubkeys []ssh.PublicKey // may or may not contain this nodes pubkey
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

func newSSHTransport(bindAddr string, raftDir string, logger *log.Logger) (*sshTransport, *raft.NetworkTransport, error) {

	s := new(sshTransport)
	s.peerPubkeys = new(peerPublicKeys)
	s.logger = logger

	// An SSH server is represented by a ServerConfig, which holds
	// certificate details and handles authentication of ServerConns.
	config := &ssh.ServerConfig{
		PublicKeyCallback: s.keyAuth,
	}

	privateBytes, err := ioutil.ReadFile(filepath.Join(raftDir, "id_rsa"))
	if err != nil {
		logger.Println("Failed to load private key, trying to generate a new pair")
		privateBytes, err = s.generateSSHKey(raftDir)

		if err != nil {
			//No usable SSH private key obtained
			return nil, nil, err
		}

	}

	private, err := ssh.ParsePrivateKey(privateBytes)
	if err != nil {
		logger.Println("Failed to parse private key:", err)
		return nil, nil, err
	}

	logger.Println("Node public key is: ", string(ssh.MarshalAuthorizedKey(private.PublicKey())))

	s.privateKey = private
	config.AddHostKey(private)

	publicKeys, err := s.readAuthorizedPeerKeys((filepath.Join(raftDir, "authorized.keys")))

	if err != nil && err != noAuthorizedPeers {
		logger.Println("Error reading authorized peer keys in newSSHTransport:", err)
		return nil, nil, err
	}

	if err == noAuthorizedPeers || len(publicKeys) < 1 {

		err := ioutil.WriteFile((filepath.Join(raftDir, "authorized.keys")), ssh.MarshalAuthorizedKey(private.PublicKey()), 0644)

		if err != nil {
			logger.Println("No public keys and error writing out new authorized key file:", err)
			return nil, nil, err
		}

		logger.Printf("Written out initial '%s', copy this key to other nodes to initialize keys\n", filepath.Join(raftDir, "authorized.keys"))

	}

	logger.Println("Parsed pubkeys", publicKeys)

	s.peerPubkeys.Lock()
	s.peerPubkeys.pubkeys = append(s.peerPubkeys.pubkeys, publicKeys...)
	s.peerPubkeys.Unlock()

	// Once a ServerConfig has been configured, connections can be
	// accepted.
	listener, err := net.Listen("tcp", bindAddr)

	if err != nil {
		logger.Println("failed to listen for connection on", bindAddr, ":", err)
		return nil, nil, err
	}

	sshClientConfig := &ssh.ClientConfig{
		User: "raft",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(private),
		},
		HostKeyCallback: s.checkHostKey,
	}

	raftListener := &streamSSHLayer{
		sshListener:  listener,
		incoming:     make(chan sshConn, 15),
		clientConfig: sshClientConfig,
		logger:       s.logger,
	}

	s.joinMessage = make(chan joinMessage)
	s.leaderMessage = make(chan leaderMessage)

	go func() {

		for {

			nConn, err := listener.Accept()

			if err != nil {
				//TODO improve failure path after closing listener
				//fix fatal for library
				log.Fatal("failed to accept incoming connection:", err)
			}

			go func() {

				// Before use, a handshake must be performed on the incoming
				// net.Conn.
				sshConnection, chans, reqs, err := ssh.NewServerConn(nConn, config)
				if err != nil {
					logger.Println("Failed to handshake:", err)
					nConn.Close()
					return
				}
				// The incoming Request channel must be serviced.
				go s.handleRequests(s.joinMessage, s.leaderMessage, reqs)

				// Service the incoming Channel channel.
				for newChannel := range chans {

					if newChannel.ChannelType() != "direct-tcpip" {
						newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
						continue
					}

					channel, requests, err := newChannel.Accept()
					if err != nil {
						logger.Println("Could not accept channel:", err)
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

	return s, raft.NewNetworkTransport(raftListener, maxPoolConnections, connectionTimeout, nil), nil

}

func (transport *sshTransport) readAuthorizedPeerKeys(path string) (pubs []ssh.PublicKey, err error) {

	//TODO Read comment and determine valid peer addresses?

	bytesRead, err := ioutil.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return
	} else if err != nil && os.IsNotExist(err) {
		err = noAuthorizedPeers
		return
	}

	if len(bytesRead) == 0 {
		return
	}

	var rest int = len(bytesRead)

	for rest > 0 {

		var pubkey ssh.PublicKey
		pubkey, _, _, bytesRead, err = ssh.ParseAuthorizedKey(bytesRead)

		if err != nil {
			transport.logger.Printf("error parsing ssh publickey from authorized peers: \n", err)
			return
		}

		pubs = append(pubs, pubkey)
		rest = len(bytesRead)

	}

	return

}

func (transport *sshTransport) handleRequests(joinChannel chan joinMessage, leaderMessageChan chan leaderMessage, reqs <-chan *ssh.Request) {

	for req := range reqs {
		transport.logger.Printf("Received out-of-band request: %+v", req)
		if req.Type == joinRequestType {

			returnChan := make(chan bool)
			msg := joinMessage{joinAddr: string(req.Payload), returnChan: returnChan}
			joinChannel <- msg

			timeout := time.After(15 * time.Second)
			select {
			case response := <-returnChan:
				err := req.Reply(response, req.Payload)
				if err != nil {
					transport.logger.Println("Error replying to join request for:", string(req.Payload))
				}
			case <-timeout:
				transport.logger.Println("Timed out processing join request for:", string(req.Payload))
				err := req.Reply(false, []byte{})
				if err != nil {
					transport.logger.Println("Error replying to join request for:", string(req.Payload))
				}
			}

			continue

		}

		if req.Type == leaderMessageType {

			returnChan := make(chan bool)

			//Decode payload

			cmd, err := deserializeCommand(req.Payload)

			if err != nil {
				transport.logger.Println("Error deserializing payload:", err)
				err := req.Reply(false, []byte{})
				if err != nil {
					transport.logger.Println("Error replying to leader request for:", string(req.Payload))
				}
			}

			msg := leaderMessage{cmd: cmd, returnChan: returnChan}
			leaderMessageChan <- msg

			timeout := time.After(connectionTimeout)
			select {
			case response := <-returnChan:
				err := req.Reply(response, []byte{})
				if err != nil {
					transport.logger.Println("Error replying to leader request for:", cmd)
				}
			case <-timeout:
				transport.logger.Println("Timed out processing leader request for:", cmd)
				err := req.Reply(false, []byte{})
				if err != nil {
					transport.logger.Println("Error replying to leader request for:", cmd)
				}
			}

			continue

		}

		transport.logger.Printf("Did not handle out of band request: %+v", req)
	}
}

func (transport *sshTransport) keyAuth(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {

	transport.logger.Println(conn.RemoteAddr(), "authenticate with", key.Type(), "for user", conn.User())

	if conn.User() != "raft" {
		return nil, errors.New("Wrong user for protocol offered by server")
	}

	transport.peerPubkeys.RLock()
	defer transport.peerPubkeys.RUnlock()

	for _, storedKey := range transport.peerPubkeys.pubkeys {

		if subtle.ConstantTimeCompare(key.Marshal(), storedKey.Marshal()) == 1 {
			return nil, nil
		}

	}

	return nil, errors.New("Public key not found")
}

func (transport *sshTransport) checkHostKey(addr string, remote net.Addr, key ssh.PublicKey) error {

	//TODO check addr

	transport.peerPubkeys.RLock()
	defer transport.peerPubkeys.RUnlock()

	for _, storedKey := range transport.peerPubkeys.pubkeys {

		if subtle.ConstantTimeCompare(key.Marshal(), storedKey.Marshal()) == 1 {
			return nil
		}

	}

	return errors.New("Public key not found")

}

func (transport *sshTransport) generateSSHKey(targetDir string) (privateKeyPem []byte, err error) {

	//generate 4096 bit rsa keypair
	var privateKey *rsa.PrivateKey
	privateKey, err = rsa.GenerateKey(rand.Reader, 4096)
	if err != nil {
		transport.logger.Println("error generating private key:", err)
		return
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
			transport.logger.Println("rrror persisting generated ssh private key:", err)
		}
	}

	return

}

type streamSSHLayer struct {
	sshListener  net.Listener
	incoming     chan sshConn
	clientConfig *ssh.ClientConfig
	logger       *log.Logger
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
