package viewservice


import "time"


const PING_INTERVAL = time.Millisecond * 100
const DEAD_PINGS = 12
const REPLICATION_LEVEL = 2
const CRITICAL_MASS = 10
const NUMBER_OF_SHARDS = 100

type View struct {
  ViewNumber uint
  ShardsToPrimaries map[int] string    // shard #{shard index} -> primary
}

type PingArgs struct {
  ServerName string
  ViewNumber uint
}

type PingReply struct {
  View View
  ServersAlive map[string] bool      // set of servers primaries can choose as backups
}

type GetArgs struct {
}

type GetReply struct {
  View View
}

type RecoveryCompletedArgs struct {
  ServerName string
  ShardRecovered int
  DataRecieved int
}

type RecoveryCompletedReply struct {

}

type StatusArgs struct {

}

type StatusReply struct {
  ServerPings       map[string] time.Time
  ServersAlive      map[string] bool
  PrimaryServers    map[string] bool
  RecoveryInProcess map[string][]int
  RecoveryMasters   map[string]map[int]bool
}


//// RPCS from pbservice

// QuerySegments

type QuerySegmentsArgs struct {
  DeadPrimaries map[string][]int
}

type QuerySegmentsReply struct {
  ServerName string
  BackedUpSegments map[string]map[int64]map[int]bool
}


// ElectRecoveryMaster

type ElectRecoveryMasterArgs struct {
  RecoveryData map[int]map[int64][]string
  DeadPrimaries map[string][]int
}

type ElectRecoveryMasterReply struct {
  ServerName string
  ShardRecovered int
}
