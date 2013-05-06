package main

import (
	"os"
	"fmt"
	"net"
	"time"
	"sort"
	"bytes"
	"errors"
	"strconv"
	"strings"
	"crypto/rand"
	"encoding/binary"
	"github.com/piotrnar/gocoin/btc"
)


const (
	Version = 70001
	UserAgent = "/Satoshi:0.8.1/"

	Services = uint64(0x1)

	SendAddrsEvery = (15*60) // 15 minutes

	MaxInCons = 8
	MaxOutCons = 8
	MaxTotCons = MaxInCons+MaxOutCons

	NoDataTimeout = 60

	MaxBytesInSendBuffer = 16*1024 // If we have more than this in the send buffer, we send no more responses
)


var (
	openCons map[uint64]*oneConnection = make(map[uint64]*oneConnection, MaxTotCons)
	InvsSent, BlockSent uint64
	InConsActive, OutConsActive uint
	
	DefaultTcpPort uint16
	MyExternalAddr *btc.NetAddr

	ConnTimeoutCnt uint64
)

type oneConnection struct {
	addr *onePeer

	last_cmd string
	
	broken bool // maker that the conenction has been broken
	ban bool // ban this client after disconnecting

	listen bool
	*net.TCPConn
	
	connectedAt int64
	ver_ack_received bool

	hdr [24]byte
	hdr_len int

	dat []byte
	datlen uint32

	sendbuf []byte
	sentsofar int

	loops, ticks uint

	invs2send []*[36]byte

	BytesReceived, BytesSent uint64

	// Data from the version message
	node struct {
		version uint32
		services uint64
		timestamp uint64
		height uint32
		agent string
	}

	NextAddrSent uint32 // When we shoudl annonce our "addr" again

	LastDataGot uint32 // if we have no data for some time, we abort this conenction
}


type BCmsg struct {
	cmd string
	pl  []byte
}


func (c *oneConnection) SendRawMsg(cmd string, pl []byte) (e error) {
	if len(c.sendbuf) > 1024*1024 {
		println(c.addr.Ip(), "WTF??", cmd, c.last_cmd)
		return
	}
	
	sbuf := make([]byte, 24+len(pl))

	c.last_cmd = cmd+"*"

	binary.LittleEndian.PutUint32(sbuf[0:4], Version)
	copy(sbuf[0:4], Magic[:])
	copy(sbuf[4:16], cmd)
	binary.LittleEndian.PutUint32(sbuf[16:20], uint32(len(pl)))

	sh := btc.Sha2Sum(pl[:])
	copy(sbuf[20:24], sh[:4])
	copy(sbuf[24:], pl)

	c.sendbuf = append(c.sendbuf, sbuf...)

	//println(len(c.sendbuf), "queued for seding to", c.addr.Ip())
	return
}


func (c *oneConnection) DoS() {
	c.ban = true
	c.broken = true
}


func putaddr(b *bytes.Buffer, a string) {
	var ip [4]byte
	var p uint16
	n, e := fmt.Sscanf(a, "%d.%d.%d.%d:%d", &ip[0], &ip[1], &ip[2], &ip[3], &p)
	if e != nil || n != 5 {
		println("Incorrect address:", a)
		os.Exit(1)
	}
	binary.Write(b, binary.LittleEndian, uint64(Services))
	// No Ip6 supported:
	b.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF})
	b.Write(ip[:])
	binary.Write(b, binary.BigEndian, uint16(p))
}


func (c *oneConnection) SendVersion() {
	b := bytes.NewBuffer([]byte{})

	binary.Write(b, binary.LittleEndian, uint32(Version))
	binary.Write(b, binary.LittleEndian, uint64(Services))
	binary.Write(b, binary.LittleEndian, uint64(time.Now().Unix()))

	putaddr(b, c.TCPConn.RemoteAddr().String())
	putaddr(b, c.TCPConn.LocalAddr().String())

	var nonce [8]byte
	rand.Read(nonce[:])
	b.Write(nonce[:])

	b.WriteByte(byte(len(UserAgent)))
	b.Write([]byte(UserAgent))

	binary.Write(b, binary.LittleEndian, uint32(LastBlock.Height))

	c.SendRawMsg("version", b.Bytes())
}


func (c *oneConnection) HandleError(e error) (error) {
	if nerr, ok := e.(net.Error); ok && nerr.Timeout() {
		//fmt.Println("Just a timeout - ignore")
		return nil
	}
	if dbg>0 {
		println("HandleError:", e.Error())
	}
	c.hdr_len = 0
	c.dat = nil
	c.broken = true
	return e
}


func (c *oneConnection) FetchMessage() (*BCmsg) {
	var e error
	var n int

	// Try for 1 millisecond and timeout if full msg not received
	c.TCPConn.SetReadDeadline(time.Now().Add(time.Millisecond))

	for c.hdr_len < 24 {
		n, e = SockRead(c.TCPConn, c.hdr[c.hdr_len:24])
		c.hdr_len += n
		if e != nil {
			c.HandleError(e)
			return nil
		}
		if c.hdr_len>=4 && !bytes.Equal(c.hdr[:4], Magic[:]) {
			println("FetchMessage: Proto out of sync")
			c.broken = true
			return nil
		}
		if c.broken {
			return nil
		}
	}

	dlen :=  binary.LittleEndian.Uint32(c.hdr[16:20])
	if dlen > 0 {
		if c.dat == nil {
			c.dat = make([]byte, dlen)
			c.datlen = 0
		}
		for c.datlen < dlen {
			n, e = SockRead(c.TCPConn, c.dat[c.datlen:])
			c.datlen += uint32(n)
			if e != nil {
				c.HandleError(e)
				return nil
			}
			if c.broken {
				return nil
			}
		}
	}

	sh := btc.Sha2Sum(c.dat)
	if !bytes.Equal(c.hdr[20:24], sh[:4]) {
		println(c.addr.Ip(), "Msg checksum error")
		c.DoS()
		c.hdr_len = 0
		c.dat = nil
		c.broken = true
		return nil
	}

	ret := new(BCmsg)
	ret.cmd = strings.TrimRight(string(c.hdr[4:16]), "\000")
	ret.pl = c.dat
	c.dat = nil
	c.hdr_len = 0

	c.BytesReceived += uint64(24+len(ret.pl))

	return ret
}


func (c *oneConnection) AnnounceOwnAddr() {
	c.SendRawMsg("getaddr", nil) // ask for his addresses BTW

	if MyExternalAddr == nil {
		return
	}
	var buf [31]byte
	now := uint32(time.Now().Unix())
	c.NextAddrSent = now+SendAddrsEvery
	buf[0] = 1 // Only one address
	binary.LittleEndian.PutUint32(buf[1:5], now)
	ipd := MyExternalAddr.Bytes()
	copy(buf[5:], ipd[:])
	c.SendRawMsg("addr", buf[:])
}


func (c *oneConnection) VerMsg(pl []byte) error {
	if len(pl) >= 46 {
		c.node.version = binary.LittleEndian.Uint32(pl[0:4])
		c.node.services = binary.LittleEndian.Uint64(pl[4:12])
		c.node.timestamp = binary.LittleEndian.Uint64(pl[12:20])
		if MyExternalAddr == nil {
			MyExternalAddr = btc.NewNetAddr(pl[20:46]) // These bytes should know our external IP
			MyExternalAddr.Port = DefaultTcpPort
		}
		if len(pl) >= 86 {
			//fmt.Println("From:", btc.NewNetAddr(pl[46:72]).String())
			//fmt.Println("Nonce:", hex.EncodeToString(pl[72:80]))
			le, of := btc.VLen(pl[80:])
			of += 80
			c.node.agent = string(pl[of:of+le])
			of += le
			if len(pl) >= of+4 {
				c.node.height = binary.LittleEndian.Uint32(pl[of:of+4])
				/*of += 4
				if len(pl) >= of+1 {
					fmt.Println("Relay:", pl[of])
				}*/
			}
		}
	} else {
		return errors.New("Version message too short")
	}
	c.SendRawMsg("verack", []byte{})
	if c.listen {
		c.SendVersion()
	}
	return nil
}


func (c *oneConnection) GetBlocks(lastbl []byte) {
	if dbg > 0 {
		println("GetBlocks since", btc.NewUint256(lastbl).String())
	}
	var b [4+1+32+32]byte
	binary.LittleEndian.PutUint32(b[0:4], Version)
	b[4] = 1 // only one locator
	copy(b[5:37], lastbl)
	// the remaining bytes should be filled with zero
	c.SendRawMsg("getblocks", b[:])
}


func (c *oneConnection) ProcessInv(pl []byte) {
	if len(pl) < 37 {
		println(c.addr.Ip(), "inv payload too short", len(pl))
		return
	}
	
	cnt, of := btc.VLen(pl)
	if len(pl) != of + 36*cnt {
		println("inv payload length mismatch", len(pl), of, cnt)
	}

	var blocks2get [][32]byte
	var txs uint32
	for i:=0; i<cnt; i++ {
		typ := binary.LittleEndian.Uint32(pl[of:of+4])
		if typ==2 {
			if InvsNotify(pl[of+4:of+36]) {
				var inv [32]byte
				copy(inv[:], pl[of+4:of+36])
				blocks2get = append(blocks2get, inv)
			}
		} else {
			txs++
		}
		of+= 36
	}
	if dbg>1 {
		println(c.addr.Ip(), "ProcessInv:", cnt, "tot /", txs, "txs -> get", len(blocks2get), "blocks")
	}
	
	if len(blocks2get) > 0 {
		msg := make([]byte, 9/*maxvlen*/+len(blocks2get)*36)
		le := btc.PutVlen(msg, len(blocks2get))
		for i := range blocks2get {
			binary.LittleEndian.PutUint32(msg[le:le+4], 2)
			copy(msg[le+4:le+36], blocks2get[i][:])
			le += 36
		}
		if dbg>0 {
			println("getdata for", len(blocks2get), "/", cnt, "blocks", le)
		}
		c.SendRawMsg("getdata", msg[:le])
	}
	return
}


func addInvBlockBranch(inv map[[32]byte] bool, bl *btc.BlockTreeNode, stop *btc.Uint256) {
	if len(inv)>=500 || bl.BlockHash.Equal(stop) {
		return
	}
	inv[bl.BlockHash.Hash] = true
	for i := range bl.Childs {
		if len(inv)>=500 {
			return
		}
		addInvBlockBranch(inv, bl.Childs[i], stop)
	}
}


func (c *oneConnection) ProcessGetBlocks(pl []byte) {
	b := bytes.NewReader(pl)
	var ver uint32
	e := binary.Read(b, binary.LittleEndian, &ver)
	if e != nil {
		println("ProcessGetBlocks:", e.Error(), c.addr.Ip())
		return
	}
	cnt, e := btc.ReadVLen(b)
	if e != nil {
		println("ProcessGetBlocks:", e.Error(), c.addr.Ip())
		return
	}
	h2get := make([]*btc.Uint256, cnt)
	var h [32]byte
	for i:=0; i<int(cnt); i++ {
		n, _ := b.Read(h[:])
		if n != 32 {
			println("getblocks too short", c.addr.Ip())
			return
		}
		h2get[i] = btc.NewUint256(h[:])
		if dbg>1 {
			println(c.addr.Ip(), "getbl", h2get[i].String())
		}
	}
	n, _ := b.Read(h[:])
	if n != 32 {
		println("getblocks does not have hash_stop", c.addr.Ip())
		return
	}
	hashstop := btc.NewUint256(h[:])

	var maxheight uint32
	invs := make(map[[32]byte] bool, 500)
	for i := range h2get {
		BlockChain.BlockIndexAccess.Lock()
		if bl, ok := BlockChain.BlockIndex[h2get[i].BIdx()]; ok {
			if bl.Height > maxheight {
				maxheight = bl.Height
			}
			addInvBlockBranch(invs, bl, hashstop)
		}
		BlockChain.BlockIndexAccess.Unlock()
		if len(invs)>=500 {
			break
		}
	}
	if len(invs) > 0 {
		inv := new(bytes.Buffer)
		btc.WriteVlen(inv, uint32(len(invs)))
		for k, _ := range invs {
			binary.Write(inv, binary.LittleEndian, uint32(2))
			inv.Write(k[:])
		}
		if dbg>1 {
			fmt.Println(c.addr.Ip(), "getblocks", cnt, maxheight, " ...", len(invs), "invs in resp ->", len(inv.Bytes()))
		}
		InvsSent++
		c.SendRawMsg("inv", inv.Bytes())
	}
}


func (c *oneConnection) ProcessGetData(pl []byte) {
	//println(c.addr.Ip(), "getdata")
	b := bytes.NewReader(pl)
	cnt, e := btc.ReadVLen(b)
	if e != nil {
		println("ProcessGetData:", e.Error(), c.addr.Ip())
		return
	}
	for i:=0; i<int(cnt); i++ {
		var typ uint32
		var h [32]byte
		
		e = binary.Read(b, binary.LittleEndian, &typ)
		if e != nil {
			println("ProcessGetData:", e.Error(), c.addr.Ip())
			return
		}

		n, _ := b.Read(h[:])
		if n!=32 {
			println("ProcessGetData: pl too short", c.addr.Ip())
			return
		}

		if typ == 2 {
			uh := btc.NewUint256(h[:])
			bl, _, er := BlockChain.Blocks.BlockGet(uh)
			if er == nil {
				BlockSent++
				c.SendRawMsg("block", bl)
			} else {
				//println("block", uh.String(), er.Error())
			}
		} else if typ == 1 {
			// transaction
			uh := btc.NewUint256(h[:])
			if tx, ok := TransactionsToSend[uh.Hash]; ok {
				c.SendRawMsg("tx", tx)
				println("sent tx to", c.addr.Ip())
			}
		} else {
			println("getdata for type", typ, "not supported yet")
		}

		if len(c.sendbuf) >= MaxBytesInSendBuffer {
			if dbg > 0 {
				println(c.addr.Ip(), "Too many bytes")
			}
			break
		}
	}
}


func (c *oneConnection) GetBlockData(h []byte) {
	var b [1+4+32]byte
	b[0] = 1 // One inv
	b[1] = 2 // Block
	copy(b[5:37], h[:32])
	if dbg > 1 {
		println("GetBlockData", btc.NewUint256(h).String())
	}
	c.SendRawMsg("getdata", b[:])
}


func (c *oneConnection) SendInvs(i2s []*[36]byte) {
	b := new(bytes.Buffer)
	btc.WriteVlen(b, uint32(len(i2s)))
	for i := range i2s {
		b.Write((*i2s[i])[:])
	}
	//println("sending invs", len(i2s), len(b.Bytes()))
	c.SendRawMsg("inv", b.Bytes())
}


func (c *oneConnection) Tick() {
	c.ticks++

	if c.sendbuf != nil {
		max2send := len(c.sendbuf) - c.sentsofar
		if max2send > 4096 {
			max2send = 4096
		}
		n, e := SockWrite(c.TCPConn, c.sendbuf[c.sentsofar:])
		if n > 0 {
			c.LastDataGot = uint32(time.Now().Unix())
			c.BytesSent += uint64(n)
			c.sentsofar += n
			//println(c.addr.Ip(), max2send, "...", c.sentsofar, n, e)
			if c.sentsofar >= len(c.sendbuf) {
				c.sendbuf = nil
				c.sentsofar = 0
			}
		}
		if e != nil {
			if dbg > 0 {
				println(c.addr.Ip(), "Connection broken during send")
			}
			c.broken = true
		}
		return
	}

	if !c.ver_ack_received {
		// If we have no ack, do nothing more.
		return
	}
	
	// Need to send getblocks...?
	if tmp := blocksNeeded(); tmp != nil {
		c.GetBlocks(tmp)
		return
	}

	// Need to send getdata...?
	if tmp := blockDataNeeded(); tmp != nil {
		c.GetBlockData(tmp)
		return
	}

	// Need to send inv...?
	var i2s []*[36]byte
	mutex.Lock()
	if len(c.invs2send)>0 {
		i2s = c.invs2send
		c.invs2send = nil
	}
	mutex.Unlock()
	if i2s != nil {
		c.SendInvs(i2s)
		return
	}

	if *server && uint32(time.Now().Unix()) >= c.NextAddrSent {
		c.AnnounceOwnAddr()
		return
	}
}


func do_one_connection(c *oneConnection) {
	if !c.listen {
		c.SendVersion()
	}

	c.LastDataGot = uint32(time.Now().Unix())
	c.NextAddrSent = c.LastDataGot + 10  // send address 10 seconds from now

	for !c.broken {
		c.loops++
		cmd := c.FetchMessage()
		if c.broken {
			break
		}
		
		now := uint32(time.Now().Unix())

		if cmd==nil {
			if int(now-c.LastDataGot) > NoDataTimeout {
				c.broken = true
				ConnTimeoutCnt++
				if dbg>0 {
					println(c.addr.Ip(), "no data for", NoDataTimeout, "seconds - disconnect")
				}
				break
			} else {
				c.Tick()
			}
			continue
		}
		
		c.LastDataGot = now
		c.last_cmd = cmd.cmd

		c.addr.Alive()

		switch cmd.cmd {
			case "version":
				er := c.VerMsg(cmd.pl)
				if er != nil {
					println("version:", er.Error())
					c.broken = true
				} else if c.listen {
					c.SendVersion()
				}

			case "verack":
				//fmt.Println("Received Ver ACK")
				c.ver_ack_received = true

			case "inv":
				c.ProcessInv(cmd.pl)
			
			case "tx": //ParseTx(cmd.pl)
				println("tx unexpected here (now)")
				c.broken = true
			
			case "addr":
				ParseAddr(cmd.pl)
			
			case "block": //block received
				netBlockReceived(c, cmd.pl)

			case "getblocks":
				if len(c.sendbuf) < MaxBytesInSendBuffer {
					c.ProcessGetBlocks(cmd.pl)
				} else if dbg>0 {
					println(c.addr.Ip(), "Ignore getblocks")
				}

			case "getdata":
				if len(c.sendbuf) < MaxBytesInSendBuffer {
					c.ProcessGetData(cmd.pl)
				} else if dbg>0 {
					println(c.addr.Ip(), "Ignore getdata")
				}

			case "getaddr":
				if len(c.sendbuf) < MaxBytesInSendBuffer {
					c.AnnounceOwnAddr()
				} else if dbg>0 {
					println(c.addr.Ip(), "Ignore getaddr")
				}

			case "alert": // do nothing

			default:
				println(cmd.cmd, "from", c.addr.Ip())
		}
	}
	if c.ban {
		c.addr.Ban()
	}
	if dbg>0 {
		println("Disconnected from", c.addr.Ip())
	}
	c.TCPConn.Close()
}


func connectionActive(ad *onePeer) (yes bool) {
	mutex.Lock()
	_, yes = openCons[ad.UniqID()]
	mutex.Unlock()
	return
}


func start_server() {
	ad, e := net.ResolveTCPAddr("tcp4", fmt.Sprint("0.0.0.0:", DefaultTcpPort))
	if e != nil {
		println("ResolveTCPAddr", e.Error())
		return
	}

	lis, e := net.ListenTCP("tcp4", ad)
	if e != nil {
		println("ListenTCP", e.Error())
		return
	}
	defer lis.Close()

	fmt.Println("TCP server started at", ad.String())

	for {
		if InConsActive < MaxInCons {
			tc, e := lis.AcceptTCP()
			if e == nil {
				if dbg>0 {
					fmt.Println("Incomming connection from", tc.RemoteAddr().String())
				}
				ad := newIncommingPeer(tc.RemoteAddr().String())
				if ad != nil {
					conn := new(oneConnection)
					conn.connectedAt = time.Now().Unix()
					conn.addr = ad
					conn.listen = true
					conn.TCPConn = tc
					mutex.Lock()
					if _, ok := openCons[ad.UniqID()]; ok {
						fmt.Println(ad.Ip(), "already connected")
						mutex.Unlock()
					} else {
						openCons[ad.UniqID()] = conn
						InConsActive++
						mutex.Unlock()
						go func () {
							do_one_connection(conn)
							mutex.Lock()
							delete(openCons, ad.UniqID())
							InConsActive--
							mutex.Unlock()
						}()
					}
				} else {
					println("newIncommingPeer failed")
					tc.Close()
				}
			}
		} else {
			time.Sleep(250e6)
		}
	}
}


func do_network(ad *onePeer) {
	var e error
	conn := new(oneConnection)
	conn.addr = ad
	mutex.Lock()
	if _, ok := openCons[ad.UniqID()]; ok {
		fmt.Println(ad.Ip(), "already connected")
		mutex.Unlock()
		return
	}
	openCons[ad.UniqID()] = conn
	OutConsActive++
	mutex.Unlock()
	go func() {
		conn.TCPConn, e = net.DialTCP("tcp4", nil, &net.TCPAddr{
			IP: net.IPv4(ad.Ip4[0], ad.Ip4[1], ad.Ip4[2], ad.Ip4[3]),
			Port: int(ad.Port)})
		if e == nil {
			conn.connectedAt = time.Now().Unix()
			if dbg>0 {
				println("Connected to", ad.Ip())
			}
			do_one_connection(conn)
		} else {
			if dbg>0 {
				println("Could not connect to", ad.Ip())
			}
			//println(e.Error())
		}
		mutex.Lock()
		delete(openCons, ad.UniqID())
		OutConsActive--
		mutex.Unlock()
		ad.Dead()
	}()
}


func network_process() {
	if *server {
		go start_server()
	}
	for {
		mutex.Lock()
		conn_cnt := OutConsActive
		mutex.Unlock()
		if conn_cnt < MaxOutCons {
			ad := getBestPeer()
			if ad != nil {
				do_network(ad)
			} else {
				if dbg>0 {
					println("no new peers", len(openCons), conn_cnt)
				}
			}
		}
		time.Sleep(250e6)
	}
}

func NetSendInv(typ uint32, h []byte, fromConn *oneConnection) (cnt uint) {
	inv := new([36]byte)
	
	binary.LittleEndian.PutUint32(inv[0:4], typ)
	copy(inv[4:36], h)
	
	mutex.Lock()
	for _, v := range openCons {
		if v != fromConn && len(v.invs2send)<500 {
			v.invs2send = append(v.invs2send, inv)
			cnt++
		}
	}
	mutex.Unlock()
	return
}


type sortedkeys []uint64

func (sk sortedkeys) Len() int {
	return len(sk)
}

func (sk sortedkeys) Less(a, b int) bool {
	return sk[a]<sk[b]
}

func (sk sortedkeys) Swap(a, b int) {
	sk[a], sk[b] = sk[b], sk[a]
}


func node_info(par string) {
	key, e := strconv.ParseUint(par, 16, 64)
	if e != nil {
		println(e.Error())
		return
	}
	mutex.Lock()
	if v, ok := openCons[key]; ok {
		fmt.Printf("Connection to node %08x:\n", key)
		if v.listen {
			fmt.Println("Comming from", v.addr.Ip())
		} else {
			fmt.Println("Going to", v.addr.Ip())
		}
		if v.connectedAt != 0 {
			now := time.Now().Unix()
			fmt.Println(" Connected:", (now-v.connectedAt), "seconds ago")
			fmt.Println(" Last data:", now-int64(v.addr.Time), "seconds ago")
			fmt.Println(" Last command:", v.last_cmd)
			fmt.Println(" Bytes received:", v.BytesReceived)
			fmt.Println(" Bytes sent:", v.BytesSent)
			fmt.Println(" Next 'addr': ", v.NextAddrSent-uint32(time.Now().Unix()), "seconds from now")
			if v.node.version!=0 {
				fmt.Println(" Node Version:", v.node.version)
				fmt.Println(" User Agent:", v.node.agent)
				fmt.Println(" Chain Height:", v.node.height)
			}
			fmt.Println(" Ticks:", v.ticks)
			fmt.Println(" Loops:", v.loops)
			if v.sendbuf != nil {
				fmt.Println(" Bytes to send:", len(v.sendbuf), "-", v.sentsofar)
			}
			if len(v.invs2send)>0 {
				fmt.Println(" Invs to send:", len(v.invs2send))
			}
		} else {
			fmt.Println("Not yet connected")
		}
	} else {
		fmt.Printf("There is no connection to node %08x\n", key)
	}
	mutex.Unlock()
}


func bts(val uint64) {
	if val < 1e5*1024 {
		fmt.Printf("%9.1f k ", float64(val)/1024)
	} else {
		fmt.Printf("%9.1f MB", float64(val)/(1024*1024))
	}
}


func net_stats(par string) {
	mutex.Lock()
	fmt.Printf("%d active net connections, %d outgoing\n", len(openCons), OutConsActive)
	srt := make(sortedkeys, len(openCons))
	cnt := 0
	for k, _ := range openCons {
		srt[cnt] = k
		cnt++
	}
	sort.Sort(srt)
	fmt.Println()
	for idx := range srt {
		v := openCons[srt[idx]]
		fmt.Printf("%4d) %08x ", idx+1, srt[idx])

		if v.listen {
			fmt.Print("<- ")
		} else {
			fmt.Print(" ->")
		}
		fmt.Printf(" %21s  %-16s", v.addr.Ip(), v.last_cmd)
		if v.connectedAt != 0 {
			bts(v.BytesReceived)
			bts(v.BytesSent)
			if v.sendbuf !=nil {
				fmt.Print("  ", v.sentsofar, "/", len(v.sendbuf))
			}
		}
		fmt.Println()
	}
	fmt.Printf("InvsSent:%d,  BlockSent:%d,  Timeouts:%d\n", 
		InvsSent, BlockSent, ConnTimeoutCnt)
	if *server && MyExternalAddr!=nil {
		fmt.Println("TCP server listening at external address", MyExternalAddr.String())
	}
	mutex.Unlock()
}


func net_drop(par string) {
	ip := net.ParseIP(par)
	if ip == nil || len(ip)!=16 {
		fmt.Println("Specify IP of the node to get disconnected")
		return
	}
	var ip4 [4]byte
	copy(ip4[:], ip[12:16])
	mutex.Lock()
	found := false
	for _, v := range openCons {
		if ip4==v.addr.Ip4 {
			v.broken = true
			found = true
			break
		}
	}
	mutex.Unlock()
	if found {
		fmt.Println("The connection is being dropped")
	} else {
		fmt.Println("You are not connected to such IP")
	}
}


func init() {
	newUi("net", false, net_stats, "Show network statistics")
	newUi("node", false, node_info, "Show information about the specific node")
	newUi("drop", false, net_drop, "Disconenct from node with a given IP")
	
}
