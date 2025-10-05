package app

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mini-Redis implementation
// Sections:
// - Global stores and configuration
// - Server setup and command execution
// - RESP parsing and writing utilities
// - List operations
// - Sorted set operations
// - RDB loading utilities

// ===== Global stores and configuration =====
var (
	kvStore   = make(map[string]string)
	expStore  = make(map[string]int64) // key -> expiry timestamp in ms since epoch
	kvMu      sync.RWMutex
	listStore = make(map[string][]string)
	listMu    sync.RWMutex
	// BLPOP waiters: per-list FIFO queue of waiting connections
	blpopWaiters = make(map[string][]blpopWaiter)
	waitersMu    sync.Mutex
	// Sorted sets storage
	zsetStore = make(map[string]*zset)
	zsetMu    sync.RWMutex
	// Config values for RDB persistence
	configDir        string
	configDBFilename string
)

// ===== Server setup and command execution =====

// RunServer starts the mini-redis server on the given address. If dir and dbfilename
// are provided, it attempts to load the RDB file before serving.
func RunServer(listenAddr string, dir string, dbfilename string) error {
	configDir = dir
	configDBFilename = dbfilename

	if configDir != "" && configDBFilename != "" {
		_ = loadRDB(configDir, configDBFilename)
	}

	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("failed to bind to %s: %w", listenAddr, err)
	}
	for {
		conn, err := l.Accept()
		if err != nil {
			fmt.Println("Error accepting connection: ", err.Error())
			continue
		}
		go handleConnection(conn)
	}
}

// handleConnection runs the command loop for a single client connection.
func handleConnection(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	for {
		parts, err := readRESPArray(reader)
		if err != nil {
			return
		}
		if len(parts) == 0 {
			continue
		}
		command := strings.ToUpper(parts[0])
		switch command {
		case "PING":
			_, _ = conn.Write([]byte("+PONG\r\n"))
		case "ECHO":
			var msg string
			if len(parts) >= 2 {
				msg = parts[1]
			}
			writeBulkString(conn, msg)
		case "SET":
			if len(parts) < 3 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'SET' command\r\n"))
				continue
			}
			key := parts[1]
			value := parts[2]
			// Optional PX expiry: SET key value PX <milliseconds>
			var pxMillis int64
			if len(parts) >= 5 && strings.ToUpper(parts[3]) == "PX" {
				if ms, err := strconv.ParseInt(parts[4], 10, 64); err == nil && ms > 0 {
					pxMillis = ms
				}
			}
			kvMu.Lock()
			kvStore[key] = value
			if pxMillis > 0 {
				expStore[key] = time.Now().Add(time.Duration(pxMillis) * time.Millisecond).UnixMilli()
			}
			kvMu.Unlock()
			writeSimpleString(conn, "OK")
		case "GET":
			if len(parts) < 2 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'GET' command\r\n"))
				continue
			}
			key := parts[1]
			kvMu.RLock()
			value, ok := kvStore[key]
			expiry := expStore[key]
			kvMu.RUnlock()
			if !ok {
				writeNullBulkString(conn)
			} else {
				// Passive expiry check
				if expiry > 0 && time.Now().UnixMilli() > expiry {
					kvMu.Lock()
					delete(kvStore, key)
					delete(expStore, key)
					kvMu.Unlock()
					writeNullBulkString(conn)
				} else {
					writeBulkString(conn, value)
				}
			}
		case "RPUSH":
			pushToList(parts, conn, "RPUSH")
		case "LPUSH":
			pushToList(parts, conn, "LPUSH")
		case "LRANGE":
			if len(parts) < 4 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'LRANGE' command\r\n"))
				continue
			}
			key := parts[1]
			start, err1 := strconv.Atoi(parts[2])
			stop, err2 := strconv.Atoi(parts[3])
			if err1 != nil || err2 != nil {
				_, _ = conn.Write([]byte("-ERR value is not an integer\r\n"))
				continue
			}
			listMu.RLock()
			list, exists := listStore[key]
			if !exists || len(list) == 0 {
				listMu.RUnlock()
				_, _ = conn.Write([]byte("*0\r\n"))
				continue
			}
			n := len(list)
			if start >= -n && start <= -1 {
				start = n + start
			}
			if stop >= -n && stop <= -1 {
				stop = n + stop
			}
			if start < 0 {
				start = 0
			}
			if stop >= n {
				stop = n - 1
			}
			if start >= n || start > stop {
				listMu.RUnlock()
				_, _ = conn.Write([]byte("*0\r\n"))
				continue
			}
			sub := list[start : stop+1]
			listMu.RUnlock()
			writeArrayOfBulkStrings(conn, sub)
		case "LLEN":
			if len(parts) < 2 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'LLEN' command\r\n"))
				continue
			}
			key := parts[1]
			listMu.RLock()
			list, ok := listStore[key]
			listMu.RUnlock()
			if !ok {
				writeInteger(conn, 0)
			} else {
				writeInteger(conn, len(list))
			}
		case "LPOP":
			if len(parts) < 2 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'LPOP' command\r\n"))
				continue
			}
			key := parts[1]
			listMu.Lock()
			list, ok := listStore[key]
			if !ok {
				listMu.Unlock()
				writeNullBulkString(conn)
			}
			if len(list) == 0 {
				listMu.Unlock()
				writeNullBulkString(conn)
			}
			if len(parts) == 3 {
				count, err := strconv.Atoi(parts[2])
				if err != nil || count <= 0 {
					_, _ = conn.Write([]byte("-ERR value is not an integer or count is out of range\r\n"))
					continue
				}
				if count > len(list) {
					count = len(list)
				}
				deletedList := list[:count]
				remainingList := list[count:]
				listStore[key] = remainingList
				listMu.Unlock()
				writeArrayOfBulkStrings(conn, deletedList)
			} else {
				removedElement := list[0]
				list = list[1:]
				listStore[key] = list
				listMu.Unlock()
				writeBulkString(conn, removedElement)
			}
		case "BLPOP":
			// Only single key and timeout supported for this stage
			if len(parts) < 3 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'BLPOP' command\r\n"))
				continue
			}
			key := parts[1]
			// Accept fractional seconds
			timeoutSeconds, err := strconv.ParseFloat(parts[2], 64)
			if err != nil || timeoutSeconds < 0 {
				_, _ = conn.Write([]byte("-ERR value is not an integer\r\n"))
				continue
			}
			// First, try to pop immediately if an element exists
			listMu.Lock()
			list := listStore[key]
			if len(list) > 0 {
				removedElement := list[0]
				list = list[1:]
				listStore[key] = list
				listMu.Unlock()
				writeArrayOfBulkStrings(conn, []string{key, removedElement})
				continue
			}
			listMu.Unlock()
			// Otherwise, enqueue as waiter
			if timeoutSeconds == 0 {
				// Indefinite block
				waitersMu.Lock()
				blpopWaiters[key] = append(blpopWaiters[key], blpopWaiter{conn: conn})
				waitersMu.Unlock()
			} else {
				w := blpopWaiter{conn: conn, ch: make(chan string, 1)}
				waitersMu.Lock()
				blpopWaiters[key] = append(blpopWaiters[key], w)
				waitersMu.Unlock()
				// Spawn timeout handler
				dur := time.Duration(timeoutSeconds * float64(time.Second))
				go timeoutHandler(key, w, dur)
			}

		case "ZADD":
			// Expect: ZADD key score member
			if len(parts) < 4 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'ZADD' command\r\n"))
				continue
			}
			key := parts[1]
			scoreStr := parts[2]
			member := parts[3]
			score, err := strconv.ParseFloat(scoreStr, 64)
			if err != nil {
				_, _ = conn.Write([]byte("-ERR value is not a valid float\r\n"))
				continue
			}
			added := zadd(key, score, member)
			writeInteger(conn, added)
		case "ZRANK":
			// Expect: ZRANK key member
			if len(parts) < 3 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'ZRANK' command\r\n"))
				continue
			}
			key := parts[1]
			member := parts[2]
			if idx, ok := zrank(key, member); ok {
				writeInteger(conn, idx)
			} else {
				writeNullBulkString(conn)
			}
		case "ZRANGE":
			// Expect: ZRANGE key start stop
			if len(parts) < 4 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'ZRANGE' command\r\n"))
				continue
			}
			key := parts[1]
			start, err1 := strconv.Atoi(parts[2])
			stop, err2 := strconv.Atoi(parts[3])
			if err1 != nil || err2 != nil {
				_, _ = conn.Write([]byte("-ERR value is not an integer\r\n"))
				continue
			}
			res := zrange(key, start, stop)
			writeArrayOfBulkStrings(conn, res)

		case "ZCARD":
			if len(parts) < 2 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'ZCARD' command\r\n"))
				continue
			}
			key := parts[1]
			writeInteger(conn, zcard(key))
		case "ZSCORE":
			if len(parts) < 3 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'ZSCORE' command\r\n"))
				continue
			}
			key := parts[1]
			member := parts[2]
			zscore(key, member, conn)
		case "ZREM":
			if len(parts) < 3 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'ZREM' command\r\n"))
				continue
			}
			key := parts[1]
			member := parts[2]
			writeInteger(conn, zrem(key, member))
		case "KEYS":
			if len(parts) < 2 {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'KEYS' command\r\n"))
				continue
			}
			pattern := parts[1]
			if pattern != "*" {
				// Only * is supported in this stage
				writeArrayOfBulkStrings(conn, []string{})
				continue
			}
			// Collect keys, skipping expired ones
			kvMu.RLock()
			keys := make([]string, 0, len(kvStore))
			nowMs := time.Now().UnixMilli()
			for k := range kvStore {
				if exp, ok := expStore[k]; ok && exp > 0 && nowMs > exp {
					continue
				}
				keys = append(keys, k)
			}
			kvMu.RUnlock()
			writeArrayOfBulkStrings(conn, keys)
		case "CONFIG":
			// Support: CONFIG GET <parameter>
			if len(parts) < 3 || strings.ToUpper(parts[1]) != "GET" {
				_, _ = conn.Write([]byte("-ERR wrong number of arguments for 'CONFIG' command\r\n"))
				continue
			}
			param := strings.ToLower(parts[2])
			var val string
			switch param {
			case "dir":
				val = configDir
			case "dbfilename":
				val = configDBFilename
			default:
				// We'll return empty array here.
				writeArrayOfBulkStrings(conn, []string{})
				continue
			}
			// Return [param, value]
			writeArrayOfBulkStrings(conn, []string{param, val})
		default:
			_, _ = conn.Write([]byte("-ERR unknown command\r\n"))
		}
	}
}

// ===== RESP parsing and writing utilities =====

// readRESPArray parses a RESP array into its string elements.
func readRESPArray(r *bufio.Reader) ([]string, error) {
	firstByte, err := r.ReadByte()
	if err != nil {
		return nil, err
	}
	if firstByte != '*' {
		return nil, fmt.Errorf("protocol error: expected array")
	}
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	numElements, err := strconv.Atoi(line)
	if err != nil {
		return nil, err
	}
	elements := make([]string, 0, numElements)
	for i := 0; i < numElements; i++ {
		b, err := r.ReadByte()
		if err != nil {
			return nil, err
		}
		if b != '$' {
			return nil, fmt.Errorf("protocol error: expected bulk string")
		}
		l, err := readLine(r)
		if err != nil {
			return nil, err
		}
		strLen, err := strconv.Atoi(l)
		if err != nil {
			return nil, err
		}
		data := make([]byte, strLen)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		// consume CRLF after bulk string data
		if cr, err := r.ReadByte(); err != nil || cr != '\r' {
			return nil, fmt.Errorf("protocol error: bad line ending after bulk string")
		}
		if lf, err := r.ReadByte(); err != nil || lf != '\n' {
			return nil, fmt.Errorf("protocol error: bad line ending after bulk string")
		}
		elements = append(elements, string(data))
	}
	return elements, nil
}

// readLine reads a CRLF-terminated line without the CRLF.
func readLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return "", fmt.Errorf("protocol error: expected CRLF line ending")
	}
	return line[:len(line)-2], nil
}

// writeBulkString writes a RESP bulk string.
func writeBulkString(w io.Writer, s string) {
	_, _ = fmt.Fprintf(w, "$%d\r\n%s\r\n", len(s), s)
}

// writeNullBulkString writes a RESP null bulk string.
func writeNullBulkString(w io.Writer) {
	_, _ = io.WriteString(w, "$-1\r\n")
}

// writeSimpleString writes a RESP simple string.
func writeSimpleString(w io.Writer, s string) {
	_, _ = fmt.Fprintf(w, "+%s\r\n", s)
}

// writeInteger writes a RESP integer.
func writeInteger(w io.Writer, n int) {
	_, _ = fmt.Fprintf(w, ":%d\r\n", n)
}

// writeArrayOfBulkStrings writes a RESP array of bulk strings.
func writeArrayOfBulkStrings(w io.Writer, items []string) {
	_, _ = fmt.Fprintf(w, "*%d\r\n", len(items))
	for _, s := range items {
		writeBulkString(w, s)
	}
}

// writeNullArray writes a RESP null array.
func writeNullArray(w io.Writer) {
	_, _ = io.WriteString(w, "*-1\r\n")
}

// ===== List operations  =====

// delivery represents a deferred response for an indefinite BLPOP waiter.
type delivery struct {
	conn net.Conn
	key  string
	elem string
}

// blpopWaiter represents a waiting client for BLPOP, with optional timer channel.
type blpopWaiter struct {
	conn net.Conn
	ch   chan string
}

// pushToList processes LPUSH/RPUSH and satisfies any waiting BLPOP clients.
func pushToList(parts []string, conn net.Conn, command string) {
	if len(parts) < 3 {
		_, _ = conn.Write([]byte("-ERR wrong number of arguments \r\n"))
		return
	}
	key := parts[1]
	elements := parts[2:]
	listMu.Lock()
	list, ok := listStore[key]
	if !ok {
		list = make([]string, 0, 1)
	}
	if command == "LPUSH" {
		reverseStringSlice(elements)
		list = append(elements, list...)
	} else {
		list = append(list, elements...)
	}
	// Reported length should be the length after the push, before satisfying any waiters
	reportedLen := len(list)

	// Satisfy oldest BLPOP waiters, if any
	deliveries := make([]delivery, 0)
	waitersMu.Lock()
	queue := blpopWaiters[key]
	for len(queue) > 0 && len(list) > 0 {
		w := queue[0]
		queue = queue[1:]
		elem := list[0]
		list = list[1:]
		if w.ch != nil {
			// Timed waiter: notify via channel
			w.ch <- elem
		} else {
			// Indefinite waiter: write response later (outside lock)
			deliveries = append(deliveries, delivery{conn: w.conn, key: key, elem: elem})
		}
	}
	if len(queue) == 0 {
		delete(blpopWaiters, key)
	} else {
		blpopWaiters[key] = queue
	}
	listStore[key] = list
	waitersMu.Unlock()
	listMu.Unlock()

	// Write RPUSH/LPUSH response first
	writeInteger(conn, reportedLen)
	// Then deliver to waiters
	for _, d := range deliveries {
		writeArrayOfBulkStrings(d.conn, []string{d.key, d.elem})
	}
}

// reverseStringSlice reverses a slice of strings in place.
func reverseStringSlice(s []string) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// timeoutHandler delivers an element to a timed waiter or times out and cleans up.
func timeoutHandler(listKey string, waiter blpopWaiter, timeout time.Duration) {
	select {
	case elem := <-waiter.ch:
		writeArrayOfBulkStrings(waiter.conn, []string{listKey, elem})
	case <-time.After(timeout):
		waitersMu.Lock()
		q := blpopWaiters[listKey]
		for i := range q {
			if q[i].conn == waiter.conn && q[i].ch == waiter.ch {
				q = append(q[:i], q[i+1:]...)
				break
			}
		}
		if len(q) == 0 {
			delete(blpopWaiters, listKey)
		} else {
			blpopWaiters[listKey] = q
		}
		waitersMu.Unlock()
		writeNullArray(waiter.conn)
	}
}

// ===== Sorted set operations =====

// zsetItem is a single sorted set entry (member, score).
type zsetItem struct {
	member string
	score  float64
}

// zset holds members and a score index; items are kept ordered.
type zset struct {
	byMember map[string]float64
	items    []zsetItem // kept ordered by (score asc, member asc)
}

// zadd inserts or updates a member with the given score; returns 1 if added, 0 if updated.
func zadd(key string, score float64, member string) int {
	zsetMu.Lock()
	defer zsetMu.Unlock()

	zs, ok := zsetStore[key]
	if !ok {
		zs = &zset{byMember: make(map[string]float64), items: make([]zsetItem, 0, 1)}
		zsetStore[key] = zs
	}
	if old, exists := zs.byMember[member]; exists {
		if old == score {
			return 0
		}
		// remove existing item
		for i := range zs.items {
			if zs.items[i].member == member {
				zs.items = append(zs.items[:i], zs.items[i+1:]...)
				break
			}
		}
	}
	insertAtCorrectIndex(score, member, zs)
	_, existed := zs.byMember[member]
	zs.byMember[member] = score
	if existed {
		return 0
	}
	return 1
}

// zrank returns the zero-based rank of member, or false if not found.
func zrank(key string, member string) (int, bool) {
	zsetMu.RLock()
	zs, ok := zsetStore[key]
	if !ok {
		zsetMu.RUnlock()
		return 0, false
	}
	if _, exists := zs.byMember[member]; !exists {
		zsetMu.RUnlock()
		return 0, false
	}
	for i, it := range zs.items {
		if it.member == member {
			zsetMu.RUnlock()
			return i, true
		}
	}
	zsetMu.RUnlock()
	return 0, false
}

// insertAtCorrectIndex inserts (member, score) to maintain sorted order by (score, member).
func insertAtCorrectIndex(score float64, member string, zs *zset) {
	// insert keeping order by score asc, then member asc
	insertIdx := len(zs.items)
	for i, it := range zs.items {
		if score < it.score || (score == it.score && member < it.member) {
			insertIdx = i
			break
		}
	}
	zs.items = append(zs.items, zsetItem{})
	copy(zs.items[insertIdx+1:], zs.items[insertIdx:])
	zs.items[insertIdx] = zsetItem{member: member, score: score}
}

// zrem removes a member from the sorted set and returns 1 if removed, 0 otherwise.
func zrem(key, member string) int {
	zsetMu.Lock()
	defer zsetMu.Unlock()
	zs, exists := zsetStore[key]
	if !exists {
		return 0
	}
	if _, exists := zs.byMember[member]; !exists {
		return 0
	}
	zs.items = removeItemFromSlice(zs.items, member)
	delete(zs.byMember, member)
	return 1
}

// removeItemFromSlice removes the first occurrence of member from items.
func removeItemFromSlice(items []zsetItem, member string) []zsetItem {
	for i, item := range items {
		if member == item.member {
			items = append(items[:i], items[i+1:]...)
			break
		}
	}
	return items
}

// zscore writes the score for member in key, or a null bulk string if missing.
func zscore(key, member string, conn net.Conn) {
	zsetMu.RLock()
	zs, exists := zsetStore[key]
	if !exists {
		zsetMu.RUnlock()
		writeNullBulkString(conn)
		return
	}
	if _, exists := zs.byMember[member]; !exists {
		zsetMu.RUnlock()
		writeNullBulkString(conn)
		return
	}
	zsetMu.RUnlock()
	writeBulkString(conn, strconv.FormatFloat(zs.byMember[member], 'f', -1, 64))
}

// zcard returns the number of items in the sorted set at key.
func zcard(key string) int {
	zsetMu.RLock()
	zs, exists := zsetStore[key]
	if !exists || len(zs.items) == 0 {
		zsetMu.RUnlock()
		return 0
	}
	zsetMu.RUnlock()
	return len(zs.items)
}

// zrange returns members in inclusive [start, stop], supporting negative indexes.
func zrange(key string, start, stop int) []string {
	zsetMu.RLock()
	zs, exists := zsetStore[key]
	if !exists || len(zs.items) == 0 {
		zsetMu.RUnlock()
		return nil
	}
	n := len(zs.items)
	if start >= -n && start <= -1 {
		start = n + start
	}
	if stop >= -n && stop <= -1 {
		stop = n + stop
	}
	if start < 0 {
		start = 0
	}
	if stop >= n {
		stop = n - 1
	}
	if start >= n || start > stop {
		zsetMu.RUnlock()
		return nil
	}
	subItems := zs.items[start : stop+1]
	members := make([]string, 0, len(subItems))
	for _, it := range subItems {
		members = append(members, it.member)
	}
	zsetMu.RUnlock()
	return members
}

// ===== RDB loading =====

// loadRDB loads keys from an RDB file into in-memory stores. Best-effort: silently returns on error.
func loadRDB(dir string, filename string) error {
	path := dir
	if !strings.HasSuffix(path, "/") {
		path += "/"
	}
	path += filename
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	// Header: "REDIS0011"
	head := make([]byte, 9)
	if _, err := io.ReadFull(r, head); err != nil {
		return err
	}
	if string(head[:5]) != "REDIS" {
		return fmt.Errorf("invalid RDB header")
	}
	// Ignore version check; assume compatible

	var pendingExpireMs int64
	for {
		b, err := r.ReadByte()
		if err != nil {
			return err
		}
		switch b {
		case 0xFA: // AUX metadata: two strings (key, value)
			if _, err := readRDBString(r); err != nil {
				return err
			}
			if _, err := readRDBString(r); err != nil {
				return err
			}
		case 0xFE: // SELECTDB: length-encoded db index
			if _, _, err := readLength(r); err != nil {
				return err
			}
		case 0xFB: // RESIZEDB: two length-encoded sizes
			if _, _, err := readLength(r); err != nil {
				return err
			}
			if _, _, err := readLength(r); err != nil {
				return err
			}
		case 0xFC: // EXPIRETIME_MS: 8-byte little-endian
			var buf [8]byte
			if _, err := io.ReadFull(r, buf[:]); err != nil {
				return err
			}
			pendingExpireMs = int64(binary.LittleEndian.Uint64(buf[:]))
		case 0xFD: // EXPIRETIME: 4-byte little-endian seconds
			var buf [4]byte
			if _, err := io.ReadFull(r, buf[:]); err != nil {
				return err
			}
			sec := int64(binary.LittleEndian.Uint32(buf[:]))
			pendingExpireMs = sec * 1000
		case 0x00: // Type: String
			key, err := readRDBString(r)
			if err != nil {
				return err
			}
			val, err := readRDBString(r)
			if err != nil {
				return err
			}
			kvMu.Lock()
			kvStore[key] = val
			if pendingExpireMs > 0 {
				expStore[key] = pendingExpireMs
			}
			kvMu.Unlock()
			pendingExpireMs = 0
		case 0xFF: // EOF, followed by 8-byte checksum
			// Ignore checksum, we're done
			return nil
		default:
			// Unknown opcode/type, stop parsing
			return nil
		}
	}
}

// readLength reads an RDB length-encoded integer. Returns (value, isEncodedString, error).
// If isEncodedString is true, the value encodes a special string format type in low 6 bits.
func readLength(r *bufio.Reader) (uint64, byte, error) {
	first, err := r.ReadByte()
	if err != nil {
		return 0, 0, err
	}
	msb := (first & 0xC0) >> 6
	switch msb {
	case 0x00: // 6-bit length
		return uint64(first & 0x3F), 0xFF, nil
	case 0x01: // 14-bit length (6 bits + next byte), big-endian
		next, err := r.ReadByte()
		if err != nil {
			return 0, 0, err
		}
		val := (uint64(first&0x3F) << 8) | uint64(next)
		return val, 0xFF, nil
	case 0x02: // 32-bit length, big-endian
		var buf [4]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return 0, 0, err
		}
		val := (uint64(buf[0]) << 24) | (uint64(buf[1]) << 16) | (uint64(buf[2]) << 8) | uint64(buf[3])
		return val, 0xFF, nil
	case 0x03: // special encoding type for strings
		return 0, first & 0x3F, nil
	}
	return 0, 0, fmt.Errorf("invalid length encoding")
}

// readRDBString reads a string-encoded value (including special integer encodings)
func readRDBString(r *bufio.Reader) (string, error) {
	length, encType, err := readLength(r)
	if err != nil {
		return "", err
	}
	if encType != 0xFF { // special integer/compressed strings
		switch encType {
		case 0x00: // 8-bit int
			b, err := r.ReadByte()
			if err != nil {
				return "", err
			}
			return strconv.FormatInt(int64(int8(b)), 10), nil
		case 0x01: // 16-bit int, little-endian
			var buf [2]byte
			if _, err := io.ReadFull(r, buf[:]); err != nil {
				return "", err
			}
			v := int16(binary.LittleEndian.Uint16(buf[:]))
			return strconv.FormatInt(int64(v), 10), nil
		case 0x02: // 32-bit int, little-endian
			var buf [4]byte
			if _, err := io.ReadFull(r, buf[:]); err != nil {
				return "", err
			}
			v := int32(binary.LittleEndian.Uint32(buf[:]))
			return strconv.FormatInt(int64(v), 10), nil
		default:
			return "", fmt.Errorf("unsupported string encoding: %d", encType)
		}
	}
	// Normal string with explicit length
	if length == 0 {
		return "", nil
	}
	buf := make([]byte, int(length))
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return string(buf), nil
}
