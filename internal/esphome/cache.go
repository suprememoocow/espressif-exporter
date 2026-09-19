package esphome

import (
	"sync"
	"time"

	"github.com/richard87/esphome-apiclient/pb"
)

// entityRecord is one entity's static metadata, learned from ListEntities.
type entityRecord struct {
	Key         uint32
	Domain      string
	ObjectID    string
	Name        string
	Unit        string
	DeviceClass string
	StateClass  int32
	Category    int32
	Icon        string
	Options     []string // for select entities
	Modes       []string // for climate entities
	Disabled    bool
}

// stateRecord is one entity's latest reading.
type stateRecord struct {
	Value     float64
	Text      string
	IsText    bool
	Missing   bool
	UpdatedAt time.Time
}

// snapshot is an immutable view of a device, served to probes without locking.
type snapshot struct {
	Connected        bool
	ConnectedSince   time.Time
	LastMessageAt    time.Time
	PingSeconds      float64
	Info             *pb.DeviceInfoResponse
	Transport        string
	Entities         map[uint32]*entityRecord
	States           map[uint32]stateRecord
	EntityGeneration uint64
	ConnectAttempts  uint64
	ConnectFailures  map[string]uint64
	StateUpdates     uint64
	Truncated        bool
	LastError        string
}

// deviceCache holds mutable device state behind a mutex.
//
// State updates are applied synchronously on the reader goroutine as an O(1) map write.
// There is deliberately no queue: if the exporter cannot keep up, TCP's receive window
// closes, the device drops us, and that is handled as an ordinary reconnect. A buffered
// channel plus a worker would add unbounded-or-lossy semantics to solve a problem the
// transport already solves.
type deviceCache struct {
	mu sync.RWMutex

	connected      bool
	connectedSince time.Time
	lastMessageAt  time.Time
	pingSeconds    float64
	info           *pb.DeviceInfoResponse
	transport      string

	entities   map[uint32]*entityRecord
	states     map[uint32]stateRecord
	generation uint64
	truncated  bool

	// orphans buffers states for keys not yet in the entity map. ESPHome sends the full
	// state dump immediately after SubscribeStates, so discarding one that lands
	// mid-swap would leave, say, a total_daily_energy blank until its next natural
	// update — which for that sensor is an hour.
	orphans    map[uint32]stateRecord
	maxOrphans int

	connectAttempts uint64
	connectFailures map[string]uint64
	stateUpdates    uint64
	lastError       string
}

func newDeviceCache(maxOrphans int) *deviceCache {
	return &deviceCache{
		entities:        map[uint32]*entityRecord{},
		states:          map[uint32]stateRecord{},
		orphans:         map[uint32]stateRecord{},
		maxOrphans:      maxOrphans,
		connectFailures: map[string]uint64{},
	}
}

// Snapshot copies the cache for a probe.
func (c *deviceCache) Snapshot() snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	states := make(map[uint32]stateRecord, len(c.states))
	for k, v := range c.states {
		states[k] = v
	}
	failures := make(map[string]uint64, len(c.connectFailures))
	for k, v := range c.connectFailures {
		failures[k] = v
	}

	return snapshot{
		Connected:      c.connected,
		ConnectedSince: c.connectedSince,
		LastMessageAt:  c.lastMessageAt,
		PingSeconds:    c.pingSeconds,
		Info:           c.info,
		Transport:      c.transport,
		// entities is replaced wholesale, never mutated, so sharing the map is safe.
		Entities:         c.entities,
		States:           states,
		EntityGeneration: c.generation,
		ConnectAttempts:  c.connectAttempts,
		ConnectFailures:  failures,
		StateUpdates:     c.stateUpdates,
		Truncated:        c.truncated,
		LastError:        c.lastError,
	}
}

// swapEntities replaces the entity map wholesale.
//
// Merging would be a bug: an entity removed by a firmware change would stay in the cache
// forever, still exported with whatever value it last had. Values are carried forward
// only for keys present in both maps — the entity key is a hash of the object ID, so it
// survives an OTA — which stops a firmware update from blanking every gauge for the few
// seconds before fresh states arrive.
func (c *deviceCache) swapEntities(next map[uint32]*entityRecord, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	carried := make(map[uint32]stateRecord, len(next))
	for key := range next {
		if s, ok := c.states[key]; ok {
			carried[key] = s
		}
		if s, ok := c.orphans[key]; ok {
			carried[key] = s
			delete(c.orphans, key)
		}
	}

	c.entities = next
	c.states = carried
	c.truncated = truncated
	c.generation++
}

func (c *deviceCache) putState(key uint32, s stateRecord) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.stateUpdates++
	c.lastMessageAt = s.UpdatedAt
	if _, known := c.entities[key]; known {
		c.states[key] = s
		return
	}
	if len(c.orphans) < c.maxOrphans {
		c.orphans[key] = s
	}
}

func (c *deviceCache) setConnected(connected bool, transport string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.connected = connected
	if connected {
		c.connectedSince = now
		c.transport = transport
		c.lastError = ""
	}
}

func (c *deviceCache) setInfo(info *pb.DeviceInfoResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.info = info
}

func (c *deviceCache) touch(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastMessageAt = now
}

func (c *deviceCache) setPing(seconds float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pingSeconds = seconds
}

func (c *deviceCache) recordAttempt() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connectAttempts++
}

func (c *deviceCache) recordFailure(reason, detail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connectFailures[reason]++
	c.lastError = detail
}
