package modbus

import (
	"encoding/binary"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/andig/mbserver"
	"github.com/evcc-io/evcc/util"
	"github.com/evcc-io/evcc/util/modbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConcurrentRead(t *testing.T) {
	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer l.Close()

	srv, _ := mbserver.New(&echoHandler{
		id:             0,
		RequestHandler: new(mbserver.DummyHandler),
	})
	require.NoError(t, srv.Start(l))
	defer func() { _ = srv.Stop() }()

	var wg sync.WaitGroup

	for id := 1; id <= 10; id++ {
		wg.Go(func() {
			// client
			conn, err := modbus.NewConnection(t.Context(), l.Addr().String(), "", "", 0, modbus.Tcp, uint8(id))
			require.NoError(t, err)

			for range 50 {
				addr := uint16(rand.N(200) + 1)
				qty := uint16(rand.N(32) + 1)

				b, err := conn.ReadInputRegisters(addr, qty)
				require.NoError(t, err)

				if err == nil {
					for u := range qty {
						assert.Equal(t, addr^uint16(id)^u, binary.BigEndian.Uint16(b[2*u:]))
					}
				}

				time.Sleep(rand.N(time.Millisecond))
			}
		})
	}

	wg.Wait()
}

func TestReadCoils(t *testing.T) {
	// downstream server
	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer l.Close()

	srv, _ := mbserver.New(&echoHandler{
		id:             0,
		RequestHandler: new(mbserver.DummyHandler),
	})
	require.NoError(t, srv.Start(l))
	defer func() { _ = srv.Stop() }()

	// proxy server
	pl, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	defer pl.Close()

	downstreamConn, err := modbus.NewConnection(t.Context(), l.Addr().String(), "", "", 0, modbus.Tcp, 1)
	require.NoError(t, err)

	proxy, _ := mbserver.New(&handler{
		log:  util.NewLogger("foo"),
		conn: downstreamConn,
	})
	require.NoError(t, proxy.Start(pl))
	defer func() { _ = proxy.Stop() }()

	// test client
	{
		conn, err := modbus.NewConnection(t.Context(), pl.Addr().String(), "", "", 0, modbus.Tcp, 1)
		require.NoError(t, err)

		{ // read
			b, err := conn.ReadCoils(1, 1)
			require.NoError(t, err)
			assert.Equal(t, []byte{0x01}, b)

			b, err = conn.ReadCoils(1, 2)
			require.NoError(t, err)
			assert.Equal(t, []byte{0x03}, b)

			b, err = conn.ReadCoils(1, 9)
			require.NoError(t, err)
			assert.Equal(t, []byte{0xFF, 0x01}, b)
		}
		{ // write
			b, err := conn.WriteSingleCoil(1, 0xFF00)
			require.NoError(t, err)
			assert.Equal(t, []byte{0xFF, 0x00}, b)

			b, err = conn.WriteMultipleCoils(1, 9, []byte{0xFF, 0x01})
			require.NoError(t, err)
			assert.Equal(t, []byte{0x00, 0x09}, b)
		}
	}
}

type echoHandler struct {
	id int
	mbserver.RequestHandler
}

func (h *echoHandler) HandleInputRegisters(req *mbserver.InputRegistersRequest) (res []uint16, err error) {
	for u := uint16(0); u < req.Quantity; u++ {
		res = append(res, req.Addr^uint16(req.UnitId)^u)
	}

	return res, err
}

func (h *echoHandler) HandleCoils(req *mbserver.CoilsRequest) (res []bool, err error) {
	if req.IsWrite {
		return nil, nil
	}

	for u := uint16(0); u < req.Quantity; u++ {
		res = append(res, true)
	}

	return res, err
}

func TestRawFunctionCodes(t *testing.T) {
	// Huawei start file upload: sub function 0x05, data length 1, file type 0x45
	req := []byte{0x05, 0x01, 0x45}

	t.Run("forwarded", func(t *testing.T) {
		conn := rawClient(t, startRawProxy(t, &rawEchoHandler{RequestHandler: new(mbserver.DummyHandler)}, ReadOnlyFalse), 16)

		fc, b, err := conn.SendRaw(0x41, req)
		require.NoError(t, err)
		assert.Equal(t, byte(0x41), fc)
		// downstream echoes unit id and request data
		assert.Equal(t, append([]byte{16}, req...), b)
	})

	t.Run("exception passed through", func(t *testing.T) {
		// Huawei permission denied
		conn := rawClient(t, startRawProxy(t, &rawExceptionHandler{RequestHandler: new(mbserver.DummyHandler), code: 0x80}, ReadOnlyFalse), 16)

		fc, b, err := conn.SendRaw(0x41, req)
		require.NoError(t, err)
		assert.Equal(t, byte(0xc1), fc)
		assert.Equal(t, []byte{0x80}, b)
	})

	for _, mode := range []ReadOnlyMode{ReadOnlyTrue, ReadOnlyDeny} {
		t.Run("rejected if read-only "+mode.String(), func(t *testing.T) {
			conn := rawClient(t, startRawProxy(t, &rawEchoHandler{RequestHandler: new(mbserver.DummyHandler)}, mode), 16)

			fc, b, err := conn.SendRaw(0x41, req)
			require.NoError(t, err)
			assert.Equal(t, byte(0xc1), fc)
			assert.Equal(t, []byte{0x01}, b) // illegal function
		})
	}
}

func TestRawConcurrentWithReads(t *testing.T) {
	// downstream answers raw requests and input register reads
	addr := startRawProxy(t, &rawEchoHandler{
		RequestHandler: &echoHandler{RequestHandler: new(mbserver.DummyHandler)},
	}, ReadOnlyFalse)

	var wg sync.WaitGroup

	for id := 1; id <= 10; id++ {
		wg.Go(func() {
			conn := rawClient(t, addr, uint8(id))

			for i := range 50 {
				if i%2 == 0 {
					// Huawei upload file frame: sub function 0x06, data length 3, file type, frame number
					data := []byte{0x06, 0x03, 0x45, byte(id), byte(i)}

					fc, b, err := conn.SendRaw(0x41, data)
					require.NoError(t, err)
					assert.Equal(t, byte(0x41), fc)
					assert.Equal(t, append([]byte{byte(id)}, data...), b)

					continue
				}

				reg := uint16(rand.N(200) + 1)
				qty := uint16(rand.N(32) + 1)

				b, err := conn.ReadInputRegisters(reg, qty)
				require.NoError(t, err)

				for u := range qty {
					assert.Equal(t, reg^uint16(id)^u, binary.BigEndian.Uint16(b[2*u:]))
				}
			}
		})
	}

	wg.Wait()
}

func rawClient(t *testing.T, addr string, id uint8) *modbus.Connection {
	t.Helper()

	conn, err := modbus.NewConnection(t.Context(), addr, "", "", 0, modbus.Tcp, id)
	require.NoError(t, err)

	return conn
}

// startRawProxy starts a downstream server using the given handler and a proxy in front of it.
// It returns the proxy address.
func startRawProxy(t *testing.T, downstream mbserver.RequestHandler, readOnly ReadOnlyMode) string {
	t.Helper()

	// downstream server
	l, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })

	srv, err := mbserver.New(downstream)
	require.NoError(t, err)
	require.NoError(t, srv.Start(l))
	t.Cleanup(func() { _ = srv.Stop() })

	// proxy server
	pl, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	t.Cleanup(func() { pl.Close() })

	downstreamConn, err := modbus.NewConnection(t.Context(), l.Addr().String(), "", "", 0, modbus.Tcp, 1)
	require.NoError(t, err)

	proxy, err := mbserver.New(&handler{
		log:      util.NewLogger("foo"),
		readOnly: readOnly,
		conn:     downstreamConn,
	})
	require.NoError(t, err)
	require.NoError(t, proxy.Start(pl))
	t.Cleanup(func() { _ = proxy.Stop() })

	return pl.Addr().String()
}

type rawEchoHandler struct {
	mbserver.RequestHandler
}

func (h *rawEchoHandler) HandleRaw(req *mbserver.RawRequest) (uint8, []byte, error) {
	return req.FunctionCode, append([]byte{req.UnitId}, req.Payload...), nil
}

type rawExceptionHandler struct {
	mbserver.RequestHandler
	code byte
}

func (h *rawExceptionHandler) HandleRaw(req *mbserver.RawRequest) (uint8, []byte, error) {
	return 0x80 | req.FunctionCode, []byte{h.code}, nil
}
