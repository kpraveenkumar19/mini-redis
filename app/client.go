package app

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
)

// runRedisCLI starts an interactive mini-redis CLI session connected to address.
// It mimics basic redis-cli behavior: prompt, send commands as RESP, and pretty-print replies.
func RunCLI(address string) error {
	conn, err := net.Dial("tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()

	reader := bufio.NewReader(conn)
	writer := bufio.NewWriter(conn)
	stdin := bufio.NewScanner(os.Stdin)
	buf := make([]byte, 0, 1024*1024)
	stdin.Buffer(buf, 1024*1024)

	for {
		fmt.Printf("mini-redis-cli> ")
		if !stdin.Scan() {
			// EOF or error on stdin: exit gracefully
			return nil
		}
		line := strings.TrimSpace(stdin.Text())
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if lower == "quit" || lower == "exit" {
			return nil
		}
		args, parseErr := splitCommandLine(line)
		if parseErr != nil || len(args) == 0 {
			fmt.Println("(error) invalid command line")
			continue
		}

		if err := writeCommand(writer, args); err != nil {
			fmt.Printf("(error) write failed: %v\n", err)
			return err
		}
		if err := writer.Flush(); err != nil {
			fmt.Printf("(error) flush failed: %v\n", err)
			return err
		}

		val, err := readRESPReply(reader)
		if err != nil {
			fmt.Printf("(error) read failed: %v\n", err)
			return err
		}
		printRESP(val)
	}
}

// writeCommand encodes args as a RESP array and writes it to writer.
func writeCommand(w *bufio.Writer, args []string) error {
	if _, err := fmt.Fprintf(w, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, a := range args {
		if _, err := fmt.Fprintf(w, "$%d\r\n%s\r\n", len(a), a); err != nil {
			return err
		}
	}
	return nil
}

// splitCommandLine splits a line on whitespace with quote handling.
func splitCommandLine(s string) ([]string, error) {
	args := make([]string, 0, 4)
	var b strings.Builder
	var quote rune // 0 when not in quotes; otherwise '\'' or '"'

	flush := func() {
		if b.Len() > 0 {
			args = append(args, b.String())
			b.Reset()
		}
	}

	for _, r := range s {
		// Handle whitespace outside quotes
		if (r == ' ' || r == '\t') && quote == 0 {
			flush()
			continue
		}
		// Handle quotes
		if r == '\'' || r == '"' {
			if quote == 0 {
				quote = r
				continue
			}
			if quote == r {
				quote = 0
				continue
			}
			// Different quote inside an active quote is treated as literal
			b.WriteRune(r)
			continue
		}
		b.WriteRune(r)
	}
	flush()
	return args, nil
}

// RESP value representation for pretty printing.
type respType int

const (
	respSimpleString respType = iota
	respError
	respInteger
	respBulkString
	respArray
	respNullBulk
	respNullArray
)

type respValue struct {
	typ     respType
	str     string
	integer int64
	array   []respValue
}

// readRESPReply parses a server reply of any RESP type.
func readRESPReply(r *bufio.Reader) (respValue, error) {
	b, err := r.ReadByte()
	if err != nil {
		return respValue{}, err
	}
	switch b {
	case '+':
		line, err := readLine(r)
		if err != nil {
			return respValue{}, err
		}
		return respValue{typ: respSimpleString, str: line}, nil
	case '-':
		line, err := readLine(r)
		if err != nil {
			return respValue{}, err
		}
		return respValue{typ: respError, str: line}, nil
	case ':':
		line, err := readLine(r)
		if err != nil {
			return respValue{}, err
		}
		n, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			return respValue{}, err
		}
		return respValue{typ: respInteger, integer: n}, nil
	case '$':
		line, err := readLine(r)
		if err != nil {
			return respValue{}, err
		}
		l, err := strconv.Atoi(line)
		if err != nil {
			return respValue{}, err
		}
		if l == -1 {
			return respValue{typ: respNullBulk}, nil
		}
		buf := make([]byte, l)
		if _, err := io.ReadFull(r, buf); err != nil {
			return respValue{}, err
		}
		// consume CRLF
		if cr, err := r.ReadByte(); err != nil || cr != '\r' {
			return respValue{}, fmt.Errorf("protocol error: bad line ending after bulk string")
		}
		if lf, err := r.ReadByte(); err != nil || lf != '\n' {
			return respValue{}, fmt.Errorf("protocol error: bad line ending after bulk string")
		}
		return respValue{typ: respBulkString, str: string(buf)}, nil
	case '*':
		line, err := readLine(r)
		if err != nil {
			return respValue{}, err
		}
		l, err := strconv.Atoi(line)
		if err != nil {
			return respValue{}, err
		}
		if l == -1 {
			return respValue{typ: respNullArray}, nil
		}
		arr := make([]respValue, 0, l)
		for i := 0; i < l; i++ {
			v, err := readRESPReply(r)
			if err != nil {
				return respValue{}, err
			}
			arr = append(arr, v)
		}
		return respValue{typ: respArray, array: arr}, nil
	default:
		return respValue{}, fmt.Errorf("protocol error: unexpected type byte %q", b)
	}
}

// printRESP pretty-prints a RESP value similar to redis-cli.
func printRESP(v respValue) {
	switch v.typ {
	case respSimpleString:
		fmt.Println(v.str)
	case respError:
		fmt.Printf("(error) %s\n", v.str)
	case respInteger:
		fmt.Printf("(integer) %d\n", v.integer)
	case respNullBulk:
		fmt.Println("(nil)")
	case respBulkString:
		fmt.Println(v.str)
	case respNullArray:
		fmt.Println("(nil)")
	case respArray:
		if len(v.array) == 0 {
			fmt.Println("(empty array)")
			return
		}
		for i, item := range v.array {
			fmt.Printf("%d) %s\n", i+1, item.str)
		}
	}
}
