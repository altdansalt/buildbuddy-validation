// A dependency-free worker implementing Bazel's JSON and delimited proto protocols.
package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

type request struct {
	Arguments []string `json:"arguments"`
	RequestID int      `json:"requestId"`
}
type observation struct {
	Arguments []string
	Content   string
	PID       int
	Count     int
}

func varint(b []byte, v uint64) []byte { return binary.AppendUvarint(b, v) }
func field(b []byte, n byte, s string) []byte {
	b = append(b, n<<3|2)
	b = varint(b, uint64(len(s)))
	return append(b, s...)
}
func decode(b []byte) request {
	var r request
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			panic("invalid tag")
		}
		b = b[n:]
		switch tag & 7 {
		case 2:
			size, n := binary.Uvarint(b)
			if n <= 0 || size > uint64(len(b)-n) {
				panic("invalid length")
			}
			v := b[n : n+int(size)]
			b = b[n+int(size):]
			if tag>>3 == 1 {
				r.Arguments = append(r.Arguments, string(v))
			}
		case 0:
			v, n := binary.Uvarint(b)
			if n <= 0 {
				panic("invalid varint")
			}
			b = b[n:]
			if tag>>3 == 3 {
				r.RequestID = int(v)
			}
		default:
			panic("unsupported wire type")
		}
	}
	return r
}
func work(r request, count int) string {
	for _, a := range r.Arguments {
		if a == "CRASH" {
			os.Exit(23)
		}
	}
	b, err := os.ReadFile("input.txt")
	if err != nil {
		panic(err)
	}
	out, err := json.Marshal(observation{r.Arguments, string(b), os.Getpid(), count})
	if err != nil {
		panic(err)
	}
	return string(out)
}
func main() {
	persistent := false
	protocol := "proto"
	for _, a := range os.Args[1:] {
		if a == "--persistent_worker" {
			persistent = true
		}
		if a == "json" {
			protocol = a
		}
	}
	if !persistent {
		b, err := os.ReadFile("args.txt")
		if err != nil {
			panic(err)
		}
		args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
		fmt.Fprint(os.Stderr, work(request{Arguments: args}, 1))
		return
	}
	in := bufio.NewReader(os.Stdin)
	dec := json.NewDecoder(in)
	for count := 1; ; count++ {
		var r request
		if protocol == "json" {
			if err := dec.Decode(&r); err == io.EOF {
				return
			} else if err != nil {
				panic(err)
			}
		} else {
			size, err := binary.ReadUvarint(in)
			if err == io.EOF {
				return
			}
			if err != nil || size > 8<<20 {
				panic("bad frame")
			}
			b := make([]byte, size)
			if _, err := io.ReadFull(in, b); err != nil {
				panic(err)
			}
			r = decode(b)
		}
		out := work(r, count)
		if protocol == "json" {
			if err := json.NewEncoder(os.Stdout).Encode(map[string]any{"exitCode": 0, "output": out, "requestId": r.RequestID}); err != nil {
				panic(err)
			}
		} else {
			b := field(nil, 2, out)
			b = append(b, 24)
			b = varint(b, uint64(r.RequestID))
			frame := varint(nil, uint64(len(b)))
			frame = append(frame, b...)
			if _, err := os.Stdout.Write(frame); err != nil {
				panic(err)
			}
		}
	}
}
