# distkv ![Travis CI](https://travis-ci.org/fabian-z/distkv.svg)  ![goreportcard](https://goreportcard.com/badge/github.com/fabian-z/distkv)

`distkv` is a distributed K/V store library for Go powered by the [raft](https://raft.github.io/) consensus algorithm. Values are only changed when a majority of nodes in the cluster agree on the operation. Internal communication is secured and powered by the SSH protocol.

It was originally based on [hraftd](https://github.com/otoolep/hraftd) by Philip O'Toole. An modified version of it is provided in `example/http-rest` as an example application built on top of `distkv`.

## Usage

Check out the `example` folder in this repository for two basic usage examples.
Some guidance is provided by [godoc](https://godoc.org/github.com/fabian-z/distkv).
API stability is not guaranteed yet, but it is unlikely to change as it is purposefully kept simple - please vendor it nonetheless.

Leader forwarding is built-in, so the application does not have to deal with the implementation of the actual distributed raft cluster. Consistent reads are going to be implemented using a separate function.

## Security

`distkv`  ensures confidentiality and security by enforcing asymmetric authentication and encryption using the SSH protocol. A custom built interface leveraging the protocols features (TCP/IP forwarding and out-of-band requests) secures all raft and control communication.

### Key distribution

**Without this step, `distkv` will not work for security reasons**

On first start, applications using `distkv` will write an `authenticated.key` file to the specified `RaftDir` (`$(pwd)/raft` if nothing is specified). This file contains the public key of this node, which has to be copied to every other nodes `authenticated.key` file. After that is done, all further cluster communication will be automatically secured. Don't forget to distributed the new key after adding a new node.

### Threat model

Every node in the cluster is inherently trusted - it has access to all data and functions (except being able to join a new node for itself). Thus it is important to secure the private key (`raftDir/id_rsa`) and apply standard best security practices (privilege separation, etc.). 

## Contribution

If you find any bugs or would like to see (or contribute to) a feature, please don't hesitate to open an issue or PR.

## License

This project is licensed under the MIT License (see `LICENSE.md`)

