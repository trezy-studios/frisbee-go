package test

import (
	"bufio"
	"fmt"
	"github.com/loophole-labs/frisbee/internal/protocol"
	"github.com/loophole-labs/frisbee/pkg/client"
	"github.com/loophole-labs/frisbee/pkg/server"
	"net"
	"testing"
)

func BenchmarkThroughput(b *testing.B) {
	addr := fmt.Sprintf("tcp://:8192")
	go server.StartServer(addr, true, true, nil)
	conn, err := net.Dial("tcp", "127.0.0.1:8192")
	if err != nil {
		panic(err)
	}

	defer conn.Close()

	bufConn := bufio.NewWriterSize(conn, 4096)

	b.Run("client-test", func(b *testing.B) {
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			for i := 0; i < 10000; i++ {
				data := []byte("BENCHMARK")
				encodedMessage, _ := client.ClientEncode(protocol.MessagePing, data)
				_, err = bufConn.Write(encodedMessage)
			}
			bufConn.Flush()
		}
	})
}