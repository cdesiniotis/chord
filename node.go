package chord

import (
	"bytes"
	"errors"
	"github.com/cdesiniotis/chord/chordpb"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"io/ioutil"
	"net"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Node implements the Chord GRPC Server interface
type Node struct {
	*chordpb.Node

	config *Config

	predecessor *chordpb.Node
	predMtx     sync.RWMutex

	successor *chordpb.Node
	succMtx   sync.RWMutex

	successorList []*chordpb.Node
	succListMtx   sync.RWMutex

	fingerTable fingerTable
	ftMtx       sync.RWMutex

	sock       *net.TCPListener
	grpcServer *grpc.Server
	grpcOpts   grpcOpts

	connPool    map[string]*clientConn
	connPoolMtx sync.RWMutex

	rgs 	map[uint64]*ReplicaGroup
	rgsMtx	sync.RWMutex
	rgFlag	int		// set to 1 initially, 0 after node sends its first Coordinator Msg

	signalChannel chan os.Signal
	shutdownCh    chan struct{}
}

// Some constants for readability
var (
	emptyNode = &chordpb.Node{}
)

/* Function: 	CreateChord
 *
 * Description:
 * 		Create a new Chord ring and return the first node
 *		in the ring.
 */
func CreateChord(config *Config) *Node {
	n := newNode(config)
	n.create()
	return n
}

/* Function: 	JoinChord
 *
 * Description:
 * 		Join an existing Chord ring. addr and port specify
 * 		an existing node in the Chord ring. Returns a newly
 * 		created node with its successor set.
 */
func JoinChord(config *Config, addr string, port int) (*Node, error) {
	n := newNode(config)
	err := n.join(&chordpb.Node{Addr: addr, Port: uint32(port)})
	if err != nil {
		log.Errorf("error joining existing chord ring: %v\n", err)
		n.shutdown()
		return nil, err
	}
	return n, err
}

/* Function: 	newNode
 *
 * Description:
 * 		Create and initialize a new node based on the config.yaml.
 * 		Start all of the necessary threads required by the
 * 		Chord protocol.
 */
func newNode(config *Config) *Node {
	// Set timestamp format for the logger
	log.SetFormatter(&log.TextFormatter{TimestampFormat: "2006-01-02 15:04:05", FullTimestamp: true})

	// Initialize some attributes
	n := &Node{
		Node:          &chordpb.Node{Addr: config.Addr, Port: config.Port},
		config:        config,
		successorList: make([]*chordpb.Node, config.SuccessorListSize),
		connPool:      make(map[string]*clientConn),
		grpcOpts: grpcOpts{
			serverOpts: config.ServerOpts,
			dialOpts:   config.DialOpts,
			timeout:    time.Duration(config.Timeout) * time.Millisecond},
		rgs:		make(map[uint64]*ReplicaGroup),
		rgFlag: 	1,
		shutdownCh:    make(chan struct{}),
		signalChannel: make(chan os.Signal, 1),
	}

	// Get PeerID
	key := n.Addr + ":" + strconv.Itoa(int(n.Port))
	n.Id = GetPeerID(key, config.KeySize)

	// Create new finger table
	n.fingerTable = NewFingerTable(n, config.KeySize)

	// Allocate a RG for us
	id := BytesToUint64(n.Id)
	n.rgs[id] = &ReplicaGroup{leaderId:n.Id, data:make(map[string][]byte)}

	// Create a listening socket for the chord grpc server
	lis, err := net.Listen("tcp", key)
	if err != nil {
		log.Fatalf("error creating listening socket %v\n", err)
	}
	n.sock = lis.(*net.TCPListener)

	// Create and register the chord grpc Server
	n.grpcServer = grpc.NewServer()
	chordpb.RegisterChordServer(n.grpcServer, n)

	// Thread 1: gRPC Server
	go func() {
		err := n.grpcServer.Serve(lis)
		if err != nil {
			log.Fatalf("error bringing up grpc server: %s\n", err)
		}
	}()

	log.Infof("Server is listening on %v\n", key)

	// Thread 2: Catch registered signals
	signal.Notify(n.signalChannel,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT)
	go func() {
		<-n.signalChannel
		n.shutdown()
		os.Exit(0)
	}()

	// Thread 3: Debug
	// Check config to check if logging is disabled
	if config.Logging == false {
		log.SetOutput(ioutil.Discard)
	} else {
		go func() {
			ticker := time.NewTicker(10 * time.Second)
			for {
				select {
				case <-ticker.C:
					log.Printf("------------\n")
					PrintNode(n.Node, false, "Self")
					PrintNode(n.predecessor, false, "Predecessor")
					PrintNode(n.successor, false, "Successor")
					PrintSuccessorList(n)
					PrintReplicaGroupMembership(n)
					n.PrintFingerTable(false)
					log.Printf("------------\n")
				case <-n.shutdownCh:
					ticker.Stop()
					return
				}
			}
		}()
	}


	// Thread 4: Stabilization protocol
	go func() {
		ticker := time.NewTicker(time.Duration(n.config.StabilizeInterval) * time.Millisecond)
		for {
			select {
			case <-ticker.C:

				n.stabilize()
			case <-n.shutdownCh:
				ticker.Stop()
				return
			}
		}
	}()

	// Thread 5: Fix Finger Table periodically
	go func() {
		next := 0
		ticker := time.NewTicker(time.Duration(n.config.FixFingerInterval) * time.Millisecond)
		for {
			select {
			case <-ticker.C:
				n.fixFinger(next)
				next = (next + 1) % n.config.KeySize
			case <-n.shutdownCh:
				ticker.Stop()
				return
			}
		}
	}()

	// Thread 6: Check health status of predecessor
	go func() {
		ticker := time.NewTicker(time.Duration(n.config.CheckPredecessorInterval) * time.Millisecond)
		for {
			select {
			case <-ticker.C:
				n.checkPredecessor()
			case <-n.shutdownCh:
				ticker.Stop()
				return
			}
		}
	}()

	return n
}

/*
 * Function:	shutdown
 *
 * Description:
 *		Gracefully shutdown a node by performing some cleanup.
 */
func (n *Node) shutdown() {
	log.Infof("In shutdown()\n")
	close(n.shutdownCh)

	log.Infof("Closing grpc server...\n")
	n.grpcServer.Stop()

	n.connPoolMtx.Lock()
	for addr, cc := range n.connPool {
		log.Infof("Closing conn %v for addr %v\n", cc, addr)
		cc.conn.Close()
		n.connPool[addr] = nil
	}
	n.connPoolMtx.Unlock()

	log.Infof("Closing listening socket\n")
	n.sock.Close()
}

/*
 * Function:	create
 *
 * Description:
 *		Create a new Chord ring. Set our predecessor to nil.
 * 		Set the successor pointer to point to ourselves.
 */
func (n *Node) create() {
	n.predMtx.Lock()
	n.predecessor = nil
	n.predMtx.Unlock()

	n.succMtx.Lock()
	n.successor = n.Node
	n.succMtx.Unlock()

	n.initSuccessorList()
}

/*
 * Function:	join
 *
 * Description:
 *		Join an existing Chord ring. Set our predecessor to nil,
 * 		but ask a node to find us our successor. "Other" is the
 *		node we know of when joining the Chord ring.
 */
func (n *Node) join(other *chordpb.Node) error {
	n.predMtx.Lock()
	n.predecessor = nil
	n.predMtx.Unlock()

	succ, err := n.FindSuccessorRPC(other, n.Id)
	if err != nil {
		log.Errorf("error calling FindSuccessorRPC(): %s\n", err)
		return err
	}

	// Get keys from successor that we are now responsible for
	kvs, err := n.GetKeysRPC(succ, n.Id)
	if err != nil {
		log.Errorf("error callling GetKeysRPC(): %v\n", err)
		return err
	}

	// Add keys to our replica group
	// On the first call to stabilize() we will initiate a leader election
	// and notify our successor list that we are the new leader
	ourId := BytesToUint64(n.Id)
	n.rgsMtx.Lock()
	for _, kv := range kvs.Kvs {
		n.rgs[ourId].data[kv.Key] = kv.Value
	}
	n.rgsMtx.Unlock()

	n.succMtx.Lock()
	n.successor = succ
	n.succMtx.Unlock()

	n.initSuccessorList()

	return nil
}

/*
 * Function:	stabilize
 *
 * Description:
 *		Implementation of the Chord stabilization protocol. Update our successor
 * 		pointer if a new node has joined between us and successor. Notify
 * 		out successor that we believe we are its predecessor.
 */
func (n *Node) stabilize() {
	/* PSEUDOCODE from paper
	x = successor.predecessor
	if (x ∈ (n, successor)) {
		successor = x
	}
	successor.notify(n)
	*/

	// Must have a successor first prior to running stabilization
	n.succMtx.RLock()
	succ := n.successor
	n.succMtx.RUnlock()

	// Don't do anything is successor isn't set
	if succ == nil {
		return
	}

	// ---- MODIFICATION TO PSEUDOCODE IN PAPER ----
	n.updateSuccessorList()
	// ---------------------------------------------

	// TODO: handle when successor fails
	// Get our successors predecessor
	x, err := n.GetPredecessorRPC(succ)
	if err != nil || x == nil {
		log.Errorf("error invoking GetPredecessorRPC: %s\n", err)
		n.removeChordClient(succ)
		return
	}

	// Update our successor if a new node joined between
	// us and our current successor
	if x.Id != nil && Between(x.Id, n.Id, succ.Id) {
		log.Infof("stabilize(): updating our successor to - %v\n", x)
		n.succMtx.Lock()
		n.successor = x
		n.succMtx.Unlock()
	}

	// Notify our successor of our existence
	n.succMtx.RLock()
	succ = n.successor
	n.succMtx.RUnlock()

	_ = n.NotifyRPC(succ)
}

/*
 * Function:	updateSuccessorList
 *
 * Description:
 *		An addition to the Chord stabilization protocol. Update the successor list
 * 		periodically. If n notices its successor has failed, it will replace
 * 		its successor with the next entry in the successor list and reconcile its
 * 		list with its new successor.
 */
func (n *Node) updateSuccessorList() {
	var succ *chordpb.Node

	index := 0
	for index < n.config.SuccessorListSize {
		n.succMtx.RLock()
		succ = n.successor
		n.succMtx.RUnlock()

		succList, err := n.GetSuccessorListRPC(succ)
		if err != nil {
			log.Errorf("successor failed while calling GetSuccessorListRPC: %v\n", err)
			// update successor the next entry in successor table
			if index == n.config.SuccessorListSize - 1 {
				break
			}
			n.succMtx.Lock()
			n.succListMtx.RLock()
			n.successor = n.successorList[index+1]
			n.succListMtx.RUnlock()
			n.succMtx.Unlock()
			index++
		} else {
			n.reconcileSuccessorList(succList)
			break
		}
	}

}

/*
 * Function:	reconcileSuccessorList
 *
 * Description:
 *		Node n reconciles its list with successor s by copying s's list,
 * 		removing the last element, and prepending s to it.
 */
func (n *Node) reconcileSuccessorList(succList *chordpb.SuccessorList) {
	n.succMtx.RLock()
	succ := n.successor
	currList := n.successorList
	n.succMtx.RUnlock()

	// Remove succList's last element
	newList := succList.Successors
	copy(newList[1:], newList)
	// Prepend our successor to the list
	newList[0] = succ

	// Update our successor list
	n.succListMtx.Lock()
	n.successorList = newList
	n.succListMtx.Unlock()

	// If successor list changed, initiate leader election
	same := CompareSuccessorLists(currList, newList)
	if !same {
		newLeaderId := n.Id
		oldLeaderId := newLeaderId

		// node just joined the chord ring
		if n.rgFlag == 1 {
			// set oldLeaderId to empty so receiving nodes know a new node has joined
			oldLeaderId = []byte{}
			n.rgFlag = 0
		}

		// Send coordinator messages to all successors (members of the replica group)
		log.Infof("In reconcileSuccessorList() - sending coordinator msg: new %d\t old: %d\n", newLeaderId, oldLeaderId)
		for _, node := range newList {
			n.RecvCoordinatorMsgRPC(node, newLeaderId, oldLeaderId)
		}

		// transfer data replicas to replica group
		n.sendAllReplicas()
	}

}

/*
 * Function:	findSuccessor
 *
 * Description:
 *		Find the successor node for the given id. First check if id ∈ (n, successor].
 *		If this is not the case then forward the request to the closest preceding node.
 */
// TODO: come back to this after implementing replica groups
func (n *Node) findSuccessor(id []byte) (*chordpb.Node, error) {
	n.succMtx.RLock()
	succ := n.successor
	n.succMtx.RUnlock()

	if BetweenRightIncl(id, n.Id, succ.Id) {
		return succ, nil
	} else {
		exclude := []*chordpb.Node{}
		n2 := n.closestPrecedingNode(id, exclude...)
		res, err := n.FindSuccessorRPC(n2, id)

		// if FindSuccessorRPC timeouts, try next best predecessor
		if err != nil {
			exclude = append(exclude, n2)
			n2 = n.closestPrecedingNode(id, exclude...)
			res, err = n.FindSuccessorRPC(n2, id)
		}

		if err != nil {
			return nil, err
		}

		return res, nil
	}
}

/*
 * Function:	closestPrecedingNode
 *
 * Description:
 *		Check finger table and find closest preceding node for a given id.
 * 		Check both the finger table and successor list. Do not return the node
 * 		if it is in the list "exclude"
 */
func (n *Node) closestPrecedingNode(id []byte, exclude ...*chordpb.Node) *chordpb.Node {
	var ftNode *chordpb.Node
	var succListNode *chordpb.Node

	// Look in finger table
	n.ftMtx.RLock()
	for i := len(id) - 1; i >= 0; i-- {
		ftEntry := n.fingerTable[i]
		if Contains(exclude, ftEntry.Node) {
			continue
		}
		if Between(ftEntry.Id, n.Id, id) {
			ftNode = n.fingerTable[i].Node
			break
		}
	}
	n.ftMtx.RUnlock()

	// Look in successor list
	n.succListMtx.RLock()
	for i := n.config.SuccessorListSize - 1; i >= 0; i--{
		succListEntry := n.successorList[i]
		if Contains(exclude, succListEntry) {
			continue
		}
		if Between(succListEntry.Id, n.Id, id) {
			succListNode = n.successorList[i]
			break
		}
	}
	n.succListMtx.RUnlock()

	// Check if no node was found in either of the lists
	if ftNode == nil && succListNode == nil {
		return n.Node
	} else if ftNode == nil {
		return succListNode
	} else if succListNode == nil {
		return ftNode
	}

	// See which node is closer to id
	if Between(ftNode.Id, succListNode.Id, id) {
		return ftNode
	} else {
		return succListNode
	}

}

/*
 * Function:	checkPredecessor
 *
 * Description:
 *		Check whether the current predecessor is still alive
 */
func (n *Node) checkPredecessor() {
	n.predMtx.RLock()
	pred := n.predecessor
	n.predMtx.RUnlock()

	if pred == nil {
		return
	}

	_, err := n.CheckPredecessorRPC(pred)
	if err != nil {
		log.Infof("detected predecessor has failed - %v\n", err)

		// transfer data to our RG before deleting it
		n.moveReplicas(BytesToUint64(pred.Id), BytesToUint64(n.Id))
		// remove membership to RG whose leader is the failed node
		id := BytesToUint64(pred.Id)
		n.removeRgMembership(id)

		// initiate new leader election
		n.succListMtx.RLock()
		succList := n.successorList
		n.succListMtx.RUnlock()
		// send coordinator msg to all
		log.Infof("In checkPredecessor() - sending coordinator msg: new %d\t old: %d\n", n.Id, pred.Id)
		for _, node := range  succList {
			n.RecvCoordinatorMsgRPC(node, n.Id, pred.Id)
		}

		// TODO: only send new keys?
		// transfer data replicas to replica group
		n.sendAllReplicas()

		// remove connection to failed predecessor
		n.removeChordClient(pred)

		n.predMtx.Lock()
		n.predecessor = nil
		n.predMtx.Unlock()
	}
}

/*
 * Function:	initSuccessorList
 *
 * Description:
 *		Initialize values of successor list to current successor. Only
 * 		used by creator of chord ring.
 */
func (n *Node) initSuccessorList() {
	n.succMtx.Lock()
	for i, _ := range n.successorList {
		n.successorList[i] = n.successor
	}
	n.succMtx.Unlock()
}

/*
 * Function:	get
 *
 * Description:
 *		Get a key's value in the datastore. First locate which
 * 		node in the ring is responsible for the key, then call
 *		GetRPC if the node is remote.
 */
func (n *Node) get(key string) ([]byte, error) {
	node, err := n.locate(key)
	if err != nil {
		return nil, err
	}

	if bytes.Compare(n.Id, node.Id) == 0 {
		// key is stored at current node
		myId := BytesToUint64(n.Id)
		n.rgsMtx.RLock()
		val, ok := n.rgs[myId].data[key]
		n.rgsMtx.RUnlock()

		if !ok {
			return nil, errors.New("key does not exist in datastore")
		}

		return val, nil
	} else {
		// key is stored at a remote node
		val, err := n.GetRPC(node, key)
		if err != nil {
			log.Errorf("error getting a key from a remote node: %s", err)
			return nil, err
		}
		return val.Value, nil
	}

}

/*
 * Function:	put
 *
 * Description:
 *		Put a key-value in the datastore. First locate which
 * 		node in the ring is responsible for the key, then call
 *		PutRPC if the node is remote.
 */
func (n *Node) put(key string, value []byte) error {
	node, err := n.locate(key)
	if err != nil {
		return err
	}

	if bytes.Compare(n.Id, node.Id) == 0 {
		// key belongs to current node

		// store kv in our datastore
		myId := BytesToUint64(n.Id)
		n.rgsMtx.RLock()
		n.rgs[myId].data[key] = value
		n.rgsMtx.RUnlock()

		// send kv to our replica group
		n.sendReplica(key)
		return nil
	} else {
		// key belongs to remote node
		_, err := n.PutRPC(node, key, value)
		return err
	}
}

/*
 * Function:	locate
 *
 * Description:
 *		Locate which node in the ring is responsible for a key.
 */
func (n *Node) locate(key string) (*chordpb.Node, error) {
	hash := GetPeerID(key, n.config.KeySize)
	node, err := n.findSuccessor(hash)
	if err != nil || node == nil {
		log.Errorf("error locating node storing the key %s with hash %d\n", key, hash)
		return nil, errors.New("error finding node storing key")
	}
	return node, nil
}
