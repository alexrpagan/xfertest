package pbservice

import (
  "net"
  "fmt"
  "net/rpc"
  "log"
  "time"
  "sync"
  "os"
  "syscall"
  "math/rand"
  "reflect"
  "unsafe"
  "path"
  "encoding/gob"
  "strconv"
  "viewservice"
)


// Operation types
const (
  GetOp = iota
  PutOp
)

// max number of bytes allowed for a segment.
const SegLimit = 8 * 1024 * 1024

// replication level for cluster
const RepLevel = 3

// number of times to retry
const Retries = 3

// absolute path to where log segments should be stored
const SegPath = "/tmp/segment/"


type PBServer struct {
  mu sync.Mutex
  l net.Listener
  dead bool // for testing
  unreliable bool // for testing
  me string

  // TODO: reference to shardmaster
  clerk *viewservice.Clerk

  view viewservice.View
  //Config shardmaster.Config

  log *Log

  // pointers to PUTs live here.
  store map[string]*Op

  // backup buffers: map each server to a Segment.
  buffers map[string]*Segment

  // which segs am I responsible for?
  backedUpSegs map[string]map[int64]bool

  // primary's backup map
  backups map[int64]BackupGroup

  // have we seen these puts?
  request map[Request]bool

  // shim of live hosts
  hosts []string

  networkMode string

}

// REQUEST

type Request struct {
  Client int64
  Request int64
}


// OPERATION

type Op struct {
//  Version VersionID
  Client int64
  Request int64
  Type int // {GetOp, PutOp}
  Key string
  Value string
}

func (op Op) size() int {
  size := int(unsafe.Sizeof(&op))  // convert from uintptr
  size += len(op.Key)
  size += len(op.Value)
  return size
}

func (op Op) Equals (diffOp Op) bool {
  return reflect.DeepEqual(op, diffOp)
}

// LOG

type Log struct {
  // TODO: only need to use a map if the int64s are non-sequential, right?
  Segments map[int64]*Segment
  CurrSegID int64
}

func (l *Log) init() {
  seg := new(Segment)
  seg.Size = 0
  seg.Active = true
  seg.ID = 1
  seg.Digest = make([]int64, 0)

  l.Segments[seg.ID] = seg
  l.CurrSegID = seg.ID
}

func (l *Log) getCurrSegment() (seg *Segment, ok bool) {
  s, o := l.Segments[l.CurrSegID]
  return s, o
}

func (l *Log) newSegment() *Segment {
  prevSegment, _ := l.getCurrSegment()
  seg := new(Segment)
  seg.Size = 0
  seg.Active = true
  seg.ID = l.CurrSegID + 1

  seg.Digest = append(prevSegment.Digest, prevSegment.ID)

  l.Segments[seg.ID] = seg
  l.CurrSegID = seg.ID

  return seg
}

// LOG SEGMENTS

type Segment struct {
  ID int64
  Active bool
  Size int  //in bytes
  Digest []int64  //the ids of all preceding segments in log
  Ops []Op
}

func (s *Segment) append (op Op) bool {
  opSize := op.size()
  if s.Size + opSize > SegLimit {
    return false
  }
  s.Ops = append(s.Ops, op)
  s.Size += opSize
  return true
}

func (s *Segment) burp (toFile string) {
  fo, err := os.Create(toFile)
  if err != nil { panic(err) }
  defer fo.Close()
  enc := gob.NewEncoder(fo)
  err = enc.Encode(s)
  if err != nil {
    panic(err)
  }
  fo.Sync()
}

func (s *Segment) slurp (fromFile string) {
  fo, err := os.Open(fromFile)
  if err != nil { panic(err) }
  defer fo.Close()
  dec := gob.NewDecoder(fo)
  err = dec.Decode(s)
  if err != nil {
    panic(err)
  }
}

// BACKUP GROUP INFO
type BackupGroup struct {
  Backups  [RepLevel] string
  Liveness [RepLevel] bool
}


func (pb *PBServer) PullSegments(args *PullSegmentsArgs, reply *PullSegmentsReply) error {
  segments := make([]Segment, len(args.Segments))
  var wg sync.WaitGroup
  for i, segId := range args.Segments {
    wg.Add(1)
    go func(i int, segId int64) {
      segment := Segment{}
      fname := strconv.Itoa(int(segId))
      segment.slurp(path.Join(SegPath, fname))
      segments[i] = segment
      wg.Done()
    }(i, segId)
  }
  wg.Wait()
  fmt.Println("xfer", args)
  reply.Segments = segments
  return nil
}

func (pb *PBServer) Get(args *GetArgs, reply *GetReply) error {
  pb.mu.Lock()
  defer pb.mu.Unlock()

  shard := key2shard(args.Key)
  // CHECK: am I the primary for this key
  if shard > 0 && false {
    reply.Err = ErrWrongServer
    return nil
  }

  op, ok := pb.store[args.Key]

  if ! ok {
    reply.Err = ErrNoKey
    return nil
  }

  reply.Value = op.Value
  reply.Err = OK
  return nil
}

func (pb *PBServer) Put(args *PutArgs, reply *PutReply) error {
  pb.mu.Lock()
  defer pb.mu.Unlock()

  seg, _ := pb.log.getCurrSegment()
  group, ok := pb.backups[seg.ID]

  if ! ok {
    if pb.enlistReplicas(*seg) == false {
      panic("could not enlist enough replicas")
    }
  }

  // create operation
  putOp := new(Op)
  putOp.Client = args.Client
  putOp.Request = args.Request
  putOp.Type = PutOp
  putOp.Key = args.Key
  putOp.Value = args.Value

  shard := key2shard(putOp.Key)
  // CHECK: am I the primary for this key
  if shard > 0 && false {
    reply.Err = ErrWrongServer
    return nil
  }

  if seg.append(*putOp) == false {
    delete(pb.backups, seg.ID)
    if pb.broadcastFlush(seg.ID, group) {
      seg = pb.log.newSegment()
      seg.append(*putOp)
      if pb.enlistReplicas(*seg) == false {
        panic("could not enlist enough replicas")
      }
    } else {
      panic("backup failure on flush")
    }
  }

  if pb.broadcastForward(*putOp, seg.ID, group) {
    pb.store[args.Key] = putOp
  } else {
    panic("backup failure on fwd")
  }

  reply.Err = OK
  return nil

}

func (pb *PBServer) checkPrimary(server string, segment int64, key string) Err {

  if key != "" {
    shard := key2shard(key)
    if shard > 0 && false {
      return ErrNotPrimary
    }
  }

  if segment != int64(0) {
    segs, _ := pb.backedUpSegs[server]
    resp, _ := segs[segment]
    if resp == false {
      return ErrNotResponsible
    }
  }

  return OK
}

// func (pb *PBServer) getReplicatedSegments(failedBackup string) ([]int64, []BackupGroup) {
//   // search through backups and return replication groups of which this server was member
// }


//TODO: fixme


func (pb *PBServer) enlistReplicas(segment Segment) bool {

  port := strconv.Itoa(SrvPort)

  hostsNeeded := RepLevel
  numHosts    := len(pb.hosts)

  availHosts := map[string]bool{}
  enlisted   := map[string]bool{}

  for i := 0; i < numHosts; i++ {
    availHosts[pb.hosts[i]] = true
  }

  enlistArgs := new(EnlistReplicaArgs)
  enlistArgs.Origin = pb.me
  enlistArgs.Segment = segment

  for {

    idxs       := rand.Perm(len(availHosts))
    candidates := make([]string, numHosts - len(enlisted))

    // copy over available hosts
    i := 0
    for key, _ := range availHosts {
      candidates[i] = key
      i++
    }

    replicaIdxs := idxs[:hostsNeeded]

    var wg sync.WaitGroup
    replies   := make([]*EnlistReplicaReply, hostsNeeded)
    acks      := make([]bool, hostsNeeded)

    to := 10 * time.Millisecond

    // for each guy who hasn't acked
    for i := 0 ; i < hostsNeeded; i++ {
      host := candidates[replicaIdxs[i]-1]
      if (acks[i] == false) {
        wg.Add(1)
        go func(i int, backup string) {
          enlistReply := new(EnlistReplicaReply)
          enlistAck   := call(backup + ":" + port, "PBServer.EnlistReplica", enlistArgs, enlistReply)
          replies[i] = enlistReply
          acks[i]    = enlistAck
          wg.Done()
        }(i, host)
      }
    }
    wg.Wait()

    for idx, ack := range acks {
      host := candidates[replicaIdxs[idx]-1]
      if ack == false {
        // TODO: what the hell do we do here?
      } else {
        reply := replies[idx]
        if (reply.Err != OK) {
          fmt.Println(reply.Err)
        } else {
          hostsNeeded--
          enlisted[host] = true
        }
        delete(availHosts, host)
      }
    }

    if hostsNeeded != 0 {

      bg := new(BackupGroup)
      i := 0
      for k, _ := range enlisted {
        bg.Backups[i]  = k
        bg.Liveness[i] = true
        i++
      }
      pb.backups[segment.ID] = *bg

      return true
    }

    time.Sleep(to)
    if to < 10 * time.Second {
      to *= 2
    }

  }
  return false
}


func (pb *PBServer) EnlistReplica(args *EnlistReplicaArgs, reply *EnlistReplicaReply) error {
  pb.mu.Lock()
  defer pb.mu.Unlock()

  segs, ok := pb.backedUpSegs[args.Origin]
  if ! ok {
    segs = map[int64]bool{}
  }

  if segs[args.Segment.ID] == false {

    segs[args.Segment.ID] = true
    pb.backedUpSegs[args.Origin] = segs

    // set segment as buffer
    pb.buffers[args.Origin] = &(args.Segment)

  } else {

    panic("replica already enlisted")

  }

  reply.Err = OK
  return nil

}

func (pb *PBServer) FlushSeg(args *FlushSegArgs, reply *FlushSegReply) error {
  pb.mu.Lock()
  defer pb.mu.Unlock()

  err := pb.checkPrimary(args.Origin, args.OldSegment, "")
  if err != OK {
    reply.Err = err
    return nil
  }

  seg, _ := pb.buffers[args.Origin]

  // write segment to disk in the background
  go func() {
    dirpath := path.Join(SegPath, args.Origin)
    os.Mkdir(dirpath, 0777)
    seg.burp(path.Join(dirpath, strconv.Itoa(int(seg.ID))))
  }()

  // no longer need this segment... will this cause problems?
  delete(pb.buffers, args.Origin)

  reply.Err = OK
  return nil
}

func (pb *PBServer) ForwardOp(args *ForwardOpArgs, reply *ForwardOpReply) error {
  pb.mu.Lock()
  defer pb.mu.Unlock()

  err := pb.checkPrimary(args.Origin, args.Segment, args.Op.Key)
  if err != OK {
    reply.Err = err
    return nil
  }

  seg, _ := pb.buffers[args.Origin]
  res := seg.append(args.Op)

  if res == false {
    panic("buffer size exceeded in replica. should never happen.")
  }

  reply.Err = OK
  return nil
}


func (pb *PBServer) broadcastForward(op Op, segment int64, group BackupGroup) bool {

  port := strconv.Itoa(SrvPort)

  numOfBackups := len(group.Backups)

  replies := make([]*ForwardOpReply, numOfBackups)
  acks    := make([]bool, numOfBackups)

  var wg sync.WaitGroup

  // set the args
  fwdArgs  := new(ForwardOpArgs)
  fwdArgs.Origin = pb.me
  fwdArgs.Op = op
  fwdArgs.Segment = segment

  for i:= 0; i < Retries; i++ {

    to := 10 * time.Millisecond

    // for each guy who hasn't acked
    for idx, backup := range group.Backups {
      if (acks[idx] == false ) {
        wg.Add(1)
        go func(idx int, backup string) {
          fwdReply := new(ForwardOpReply)
          ack := call(backup + ":" + port, "PBServer.ForwardOp", fwdArgs, fwdReply)
          replies[idx] = fwdReply
          acks[idx]    = ack
          wg.Done()
        }(idx, backup)
      }
    }
    wg.Wait()

    numAcked := 0

    // process the responses.
    for idx, ack := range acks {
      if ack == false {
        // TODO: what the hell do we do here?
      } else {
        reply := replies[idx]
        if (reply.Err != OK) {
          return false
        } else {
          numAcked += 1
        }
      }
    }

    if numAcked == len(group.Backups) {
      return true
    }

    time.Sleep(to)
    if to < 10 * time.Second {
      to *= 2
    }

  }

  //wasnt able to ack everyone
  return false
}


func (pb *PBServer) broadcastFlush(segment int64, group BackupGroup) bool {

  port := strconv.Itoa(SrvPort)

  replies := make([]*FlushSegReply, len(group.Backups))
  acks    := make([]bool, len(group.Backups))

  // set the args
  flshArgs  := new(FlushSegArgs)
  flshArgs.Origin = pb.me
  flshArgs.OldSegment = segment

  for i:= 0; i < Retries; i++ {

    to := 10 * time.Millisecond
    count := 0

    // for each guy who hasn't acked
    for idx, backup := range group.Backups {
      if (acks[idx] == false ) {
        count += 1
        go func(idx int, backup string) {
          flshReply := new(FlushSegReply)
          ack := call(backup + ":" + port, "PBServer.FlushSeg", flshArgs, flshReply)
          replies[idx] = flshReply
          acks[idx] = ack
        }(idx, backup)
      }
    }

    numAcked := 0

    // process the responses.
    for idx, ack := range acks {
      if ack == false {

      } else {
        reply := replies[idx]
        if (reply.Err != OK) {
          return false
        } else {
          numAcked += 1
        }
      }
    }

    if numAcked == len(group.Backups) {
      return true
    }

    time.Sleep(to)
    if to < 10 * time.Second {
      to *= 2
    }
  }

  //wasnt able to ack everyone
  return false
}



func (pb *PBServer) tick() {

	view, err := pb.clerk.Ping(pb.view.ViewNumber)

  if err == nil {
    pb.view = view
  }

}


// for performance testing. Isolate particular ops.
func (pb *PBServer) TestWriteSegment(args *TestWriteSegmentArgs, reply *TestWriteSegmentReply) error {
  var wg sync.WaitGroup
  os.Mkdir(SegPath, 0777)
  for i := 0; i < args.NumOfSegs; i++ {
    wg.Add(1)
    go func(i int) {
      segment := Segment{}
      segment.ID = int64(i)
      for {
        op := Op{}
        op.Client = int64(0)
        op.Request = int64(0)
        op.Type = PutOp
        op.Key = "foo foo foo foo foo foo foo foo foo foo"
        op.Value = "bar bar bar bar bar bar bar bar bar bar"
        if segment.append(op) == false {
          break
        }
      }
      fname := strconv.Itoa(i)
      segment.burp(path.Join(SegPath, fname))
      wg.Done()
    }(i)
  }
  wg.Wait()
  return nil
}

func (pb *PBServer) TestReadSegment(args *TestReadSegmentArgs, reply *TestReadSegmentReply) error {
  var wg sync.WaitGroup
  for i := 0; i < args.NumOfSegs; i++ {
    wg.Add(1)
    go func(i int) {
      segment := Segment{}
      fname := strconv.Itoa(i)
      segment.slurp(path.Join(SegPath, fname))
      wg.Done()
    }(i)
  }
  wg.Wait()
  return nil
}

func (pb *PBServer) TestPullSegments(args *TestPullSegmentsArgs, reply *TestPullSegmentsReply) error {
  var wg sync.WaitGroup
  port := strconv.Itoa(SrvPort)
  t1 := time.Now().UnixNano()

  for _, host := range args.Hosts {
    if host == "" {
      continue
    }
    for cnt:=0; cnt < 300; cnt++ {
      wg.Add(1)
      go func(host string) {
        sendargs  := new(PullSegmentsArgs)
        sendreply := new(PullSegmentsReply)
        sendargs.Segments = make([]int64, 1)
        sendargs.Segments[0] = int64(rand.Int63() % 30)
        ok := call(host + ":" + port, "PBServer.PullSegments", sendargs, sendreply)
        if ok {
          fmt.Println("segment from", host)
        }
        wg.Done()
      }(host)
    }
  }
  wg.Wait()
  t2 := time.Now().UnixNano()
  fmt.Println(t2-t1)
  return nil
}




// tell the server to shut itself down.
// please do not change this function.
func (pb *PBServer) kill() {
  pb.dead = true
  pb.l.Close()
}

func StartServer(me string, viewServer string) *PBServer {
  return StartMe(me, viewServer, "unix")
}

func StartMe(me string, viewServer string, networkMode string) *PBServer {

  pb := new(PBServer)
  pb.me = me

  pb.view = viewservice.View{}

  pb.clerk = viewservice.MakeClerk(me, viewServer)

  // initialize main data structures
  pb.log = new(Log)

  pb.store = map[string]*Op{}

  pb.buffers = map[string]*Segment{}

  pb.backedUpSegs = map[string]map[int64]bool{}

  pb.backups = map[int64]BackupGroup{}

  pb.networkMode = networkMode

  rpcs := rpc.NewServer()
  rpcs.Register(pb)

  if networkMode == "unix" {
    os.Remove(pb.me)
  }

  l, e := net.Listen(networkMode, pb.me);

  if e != nil {
    log.Fatal("listen error: ", e);
  }
  pb.l = l

  // please do not change any of the following code,
  // or do anything to subvert it.

  go func() {
    for pb.dead == false {
      conn, err := pb.l.Accept()
      if err == nil && pb.dead == false {

        // deaf!
        if pb.unreliable && (rand.Int63() % 1000) < 100 {
          // discard the request.
          conn.Close()

        // mute!
        } else if pb.unreliable && (rand.Int63() % 1000) < 200 {
          // process the request but force discard of reply.
          c1 := conn.(*net.UnixConn)
          f, _ := c1.File()
          err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
          if err != nil {
            fmt.Printf("shutdown: %v\n", err)
          }
          go rpcs.ServeConn(conn)

        // healthy!
        } else {
          go rpcs.ServeConn(conn)
        }

      } else if err == nil {
        conn.Close()
      }

      if err != nil && pb.dead == false {
        fmt.Printf("PBServer(%v) accept: %v\n", me, err.Error())
        pb.kill()
      }
    }
  }()

  go func() {
    for pb.dead == false {
      pb.tick()
      time.Sleep(100 * time.Millisecond)
      //time.Sleep(viewservice.PingInterval)
    }
  }()

  return pb
}


// local version of PullSegments for use in QuerySegments
func (pb *PBServer) PullSegmentLocal(segmentID int64) Segment {
  segment := Segment{}
  fname := strconv.Itoa(int(segmentID))
  segment.slurp(path.Join(SegPath, fname))
  return segment
}


// tell the viewserver which shards you have segments for and which segments you have
func (pb *PBServer) QuerySegments(args *QuerySegmentsArgs, reply *QuerySegmentsReply) error {

	reply.ServerName = pb.me
	reply.ShardsToSegments = make(map[int] (map[int64] bool))

	// for each server, grab the segments belonging to that server and add them to the appropriate shards in reply.ShardsToSegments
	for deadPrimary, _ := range args.DeadPrimaries {

		for segmentID, _ := range pb.backedUpSegs[deadPrimary] {

			segment := pb.PullSegmentLocal(segmentID)

			for _, op := range segment.Ops {

				shard := key2shard(op.Key)

				_, present := reply.ShardsToSegments[shard]

				if !present {

					reply.ShardsToSegments[shard] = make(map[int64] bool)

				}

				reply.ShardsToSegments[shard][segmentID] = true

			}

		}

	}

	return nil

}


func (pb *PBServer) PullSegmentsByShards(args *PullSegmentsByShardsArgs, reply *PullSegmentsByShardsReply) error {
  segments := make([]Segment, len(args.Segments))
  var wg sync.WaitGroup
  for i, segId := range args.Segments {
    wg.Add(1)
    go func(i int, segId int64) {
      segment := Segment{}
      fname := strconv.Itoa(int(segId))
      segment.slurp(path.Join(SegPath, fname))

      j := 0

      for _, op := range segment.Ops {

       	if args.Shards[key2shard(op.Key)] {

      		segment.Ops[j] = op
      		j++

      	}

      }

      segment.Ops = segment.Ops[:j]

      segments[i] = segment
      wg.Done()
    }(i, segId)
  }
  wg.Wait()
  fmt.Println("xfer", args)
  reply.Segments = segments
  return nil
}


// recover the shards in args.ShardsToSegmentsToServers
func (pb *PBServer) ElectRecoveryMaster(args *ElectRecoveryMasterArgs, reply *ElectRecoveryMasterReply) error {

	reply.ServerName = pb.me

  var repliesFinishedLock sync.Mutex

	// while there are still shards to recover, assemble and execute queryPlans for segments
	for len(args.ShardsToSegmentsToServers) > 0 {

		// map from backups to log segments for querying
		queryPlan := make(map[string] (map[int64] bool))
		// map from segments to shards to keep track of completed shards
		segmentShards := make(map[int64] int)
		// keep track of how many replies have finished so we can wait for one query to complete before beginning the next
		repliesFinished := 0

		// assemble queryPlan by assigning segments to the server with the smallest queryPlan so far
		for shard, segmentsToServers := range args.ShardsToSegmentsToServers {

			for segment, servers := range segmentsToServers {

				// if all the backups for this segment have failed, just ignore it and move on
				// for keys where this is not the final segment, they will not be affect
				// for keys where this is the final segment among several, their values will revert to the last recoverable value
				// for keys where this is the only segment, the data will be completely lost
				if len(servers) <= 0 {

					delete(args.ShardsToSegmentsToServers[shard], segment)

				// otherwise, we have backups and should proceed normally
				} else {

					// keep track of which shards segments belong to
					segmentShards[segment] = shard

					// find which server has the shortest query plan so far and assign to it this segment
					min := -1
					var minServer string

					for server, _ := range servers {

						if min < 0 || len(queryPlan[server]) < min {

							min = len(queryPlan[server])
							minServer = server

						}

					}

					_, present := queryPlan[minServer]

					// notice we only at the server to the queryPlan once we know it's part of the query
					// we don't create a map prematurely both for simplicity and for knowledge of how many replies we're waiting for before making another query
					if !present {

						queryPlan[minServer] = make(map[int64] bool)

					}

					queryPlan[minServer][segment] = true
					// remove servers from segments as they are tried so we won't retry a server twice
					// note: this doesn't remove the server for ALL segments, but that does happen in the goroutine below for failed servers
					delete(args.ShardsToSegmentsToServers[shard][segment], minServer)

				}

			}

		}

		// synchronize around the decision phases of the separate query threads
		var queryLock sync.Mutex
		// most recent recovered puts by key to allow replaying of more recent puts from recovered log segments
		keysToPuts := make(map[string] PutOrder)
		// keep track of segments which have been recovered so we don't repeat iterating through them for future shards which share them
		recoveredSegments := make(map[int64] bool)

		// convert args.ShardsToSegmentsToServers to set of shards for use by PBServer.PullSegmentsByShards
		shardsToRecover := make(map[int] bool)

		for shard, _ := range args.ShardsToSegmentsToServers {

			shardsToRecover[shard] = true

		}

		// execute the queryPlan by pulling the segments from each of the servers
		for server, segments := range queryPlan {

			// convert segments to a slice for sending
			segmentsToRequest := make([]int64, len(segments))

			i := 0

			for segment, _ := range segments {

				segmentsToRequest[i] = segment

				i++

			}

			// launch a goroutine to request the segments, replay the relevant log segments, and prune args.ShardsToSegmentsToServers
			go func(backupServer string, requestedSegments []int64) {

				// keep track of which shards have recovered successfully for reporting to the viewservice
				completedShards := make(map[int] bool)

				// request the segments
				pullSegmentsByShardsArgs := PullSegmentsByShardsArgs{Segments: requestedSegments, Shards: shardsToRecover}
				pullSegmentsByShardsReply := PullSegmentsByShardsReply{}
				successful := call(server, "PBServer.PullSegmentsByShards", pullSegmentsByShardsArgs, &pullSegmentsByShardsReply)

				// grab the queryLock around the entire decision phase
				queryLock.Lock()

				// upon receiving a reply, iterate through the segments, replay most recent ones, and remove them from args.ShardsToSegmentsToSenders
				if successful {

					for _, segment := range pullSegmentsByShardsReply.Segments {

						// check for segments in recoveredSegments
						if !recoveredSegments[segment.ID] {

							// replay put operations if they are more recent than other recovered puts for the same key
							for opIndex, op := range segment.Ops {

								_, present := keysToPuts[op.Key]

								// if !present || (segment.ID greater || (segment.ID equal but opIndex greater)), replay, append, and update
								if !present || (segment.ID > keysToPuts[op.Key].SegmentID || (segment.ID == keysToPuts[op.Key].SegmentID && opIndex > keysToPuts[op.Key].OpIndex)) {

									pb.mu.Lock()

                  currSeg, _ := pb.log.getCurrSegment()

                  // TODO: what if group is not initialized?
                  group, _ := pb.backups[currSeg.ID]

									if !currSeg.append(op) {

										delete(pb.backups, currSeg.ID)

										if pb.broadcastFlush(currSeg.ID, group) {

											currSeg = pb.log.newSegment()
											currSeg.append(op)

											if pb.enlistReplicas(*currSeg) == false {

												panic("could not enlist enough replicas")

											}

										} else {

											panic("backup failure on flush")

										}

									}

									pb.mu.Unlock()

									keysToPuts[op.Key] = PutOrder{SegmentID: segment.ID, OpIndex: opIndex}

								}

							}

							// mark the segment as recovered so we won't iterate through it again for future shards
							recoveredSegments[segment.ID] = true

						}

						// prune segments from args.ShardsToSegmentsToSenders
						delete(args.ShardsToSegmentsToSenders[segmentShards[segment.ID]], segment.ID)

						// if all of the shard's segments have been recovered, call ViewServer.RecoveryCompleted and prune the shard
						if len(args.ShardsToSegmentsToServers[segmentShards[segment.ID]]) <= 0 {

							completedShards[segmentShards[segment.ID]] = true
							// remove the shard from args.ShardsToSegmentsToServers so the next queryPlan won't try to recover it
							delete(args.ShardsToSegmentsToServers, segmentShards[segment.ID])

						}

					}

				} else {

					// consider the server failed, remove it from all segments in args.ShardsToSegmentsToServers
					for shard, segmentsToServers := range args.ShardsToSegmentsToServers {

						for segment, _ := range segmentsToServers {

							delete(args.ShardsToSegmentsToServers[shard][segment], backupServer)

						}

					}

				}

				repliesFinished++
				// release the queryLock
				queryLock.Unlock()

				// report any completed shards to the viewservice
				for completedShard, _ := range completedShards {

					recoveryCompleted := false

					for !recoveryCompleted {

            // TODO: should probably encapulate in clerk

						recoveryCompletedArgs := RecoveryCompletedArgs{ServerName: pb.me, ShardRecovered: completedShard}
						recoveryCompletedReply := RecoveryCompletedReply{}
						recoveryCompleted = call(pb.clerk.GetServerName(), "ViewServer.RecoveryCompleted", &recoveryCompletedArgs, &recoveryCompletedReply)

					}

				}

			}(server, segmentsToRequest)

		}

		// should wait for the query to complete before issuing duplicate rpc's in a new query
		// possible optimization: it would be nice if instead of thinking in terms of full queries,
		// we could start asking for unretrieved segments without waiting
		// we resorted to the full queryPlan model because we originally sent an rpc for each individual segment
		// this obviously created a ridiculous amount of rpc's to each server
		// some compromise of the two ideas would be best
		// for example, do 1 iteration of a full queryPlan, and as segments fail retrieval, they are then sent for individually,
		// or batched with some timing mechanism
		// the optimization of this scheme could get pretty elaborate, so for now we are happy with the iterative queryPlan idea
		// furthermore, there is actually no difference between these ideas if no servers fail during the first query execution,
		// which our failure model assumes is highly likely
		for {

			time.Sleep(PULL_SEGMENTS_SLEEP_INTERVAL)

			repliesFinishedLock.Lock()

			if repliesFinished >= len(queryPlan) {

				repliesFinishedLock.Unlock()

				break

			}

			repliesFinishedLock.Unlock()

		}

	}

	return nil

}
