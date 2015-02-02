package main

import (
	dhcp "github.com/justinsb/dhcp4"

	"flag"
	"log"
	"net"
	"reflect"
	"syscall"
	"time"
)

var flagRouter = flag.String("router", "", "router to offer over DHCP")
var flagSubnet = flag.String("subnet", "", "subnet to offer over DHCP")
var flagDns = flag.String("dns", "", "dns to offer over DHCP")
var flagMac = flag.String("mac", "", "base MAC address")
var flagServerIP = flag.String("serverip", "", "DHCP server IP address")
var flagNetIf = flag.String("netif", "", "Network interface to listen")

// Example using DHCP with a single network interface device
func main() {
	flag.Parse()

	options := dhcp.Options{}

	if flagRouter != nil && *flagRouter != "" {
		ip := net.ParseIP(*flagRouter)
		if ip == nil {
			log.Fatal("Unable to parse router:" + *flagRouter)
		}
		ip = ip.To4()
		if ip == nil {
			log.Fatal("Expected IPv4 address for router")
		}
		options[dhcp.OptionRouter] = []byte(ip)
	}

	if flagDns != nil && *flagDns != "" {
		ip := net.ParseIP(*flagDns)
		if ip == nil {
			log.Fatal("Unable to parse dns:" + *flagDns)
		}
		ip = ip.To4()
		if ip == nil {
			log.Fatal("Expected IPv4 address for dns")
		}
		options[dhcp.OptionDomainNameServer] = []byte(ip)
	}
	handler := &DHCPHandler{
		//		ip:            serverIP,
		//		baseHwaddr:    baseHwaddr,
		leaseDuration: 24 * time.Hour,
	}

	if flagSubnet == nil || *flagSubnet == "" {
		log.Fatal("subnet is required")
	} else {
		ip, net, err := net.ParseCIDR(*flagSubnet)
		if err != nil {
			log.Fatal("Unable to parse subnet:" + *flagSubnet)
		}
		ip = ip.To4()
		if ip == nil {
			log.Fatal("Expected IPv4 address for subnet")
		}
		options[dhcp.OptionSubnetMask] = []byte(net.Mask)
		if len(net.Mask) != 4 {
			panic("Unexpected netmask length")
		}
		handler.baseIP = ip
		handler.netmask = net.Mask
	}

	if flagMac == nil || *flagMac == "" {
		log.Fatal("mac is required")
	} else {
		baseHwaddr, err := net.ParseMAC(*flagMac)
		if err != nil {
			log.Fatal("Unable to parse mac:" + *flagMac)
		}
		handler.baseHwaddr = baseHwaddr
	}

	if flagServerIP == nil || *flagServerIP == "" {
		log.Fatal("serverip is required")
	} else {
		ip := net.ParseIP(*flagServerIP)
		if ip == nil {
			log.Fatal("Unable to parse serverip:" + *flagServerIP)
		}
		ip = ip.To4()
		if ip == nil {
			log.Fatal("Expected IPv4 address for subnet")
		}
		handler.serverIP = ip
	}

	handler.options = options
	handler.netif = *flagNetIf

	log.Println("Starting DHCP server")
	log.Fatal(handler.listenAndServe())
	// log.Fatal(dhcp.ListenAndServeIf("eth0",handler)) // Select interface on multi interface device
}

type lease struct {
	nic    string    // Client's CHAddr
	expiry time.Time // When the lease expires
}

type DHCPHandler struct {
	baseHwaddr net.HardwareAddr
	baseIP     net.IP
	netmask    net.IPMask

	netif string

	serverIP net.IP       // Server IP to use
	options  dhcp.Options // Options to send to DHCP Clients
	//	start         net.IP        // Start of IP range to distribute
	//	leaseRange    int           // Number of IPs to distribute (starting from start)
	leaseDuration time.Duration // Lease period
	//	leases        map[int]lease // Map to keep track of leases
}

func bindToIf(conn net.PacketConn, interfaceName string) {
	ptrVal := reflect.ValueOf(conn)
	val := reflect.Indirect(ptrVal)
	//next line will get you the net.netFD
	fdmember := val.FieldByName("fd")
	val1 := reflect.Indirect(fdmember)
	netFdPtr := val1.FieldByName("sysfd")
	fd := int(netFdPtr.Int())
	//fd now has the actual fd for the socket
	err := syscall.SetsockoptString(fd, syscall.SOL_SOCKET,
		0x19/*syscall.SO_BINDTODEVICE*/, interfaceName)
	if err != nil {
		log.Fatal(err)
	}
}

func (self *DHCPHandler) listenAndServe() error {
	l, err := net.ListenPacket("udp4", ":67")
	if err != nil {
		return err
	}
	defer l.Close()
	if self.netif != "" {
		bindToIf(l, self.netif)
	}
	return dhcp.Serve(l, self)
}
func (self *DHCPHandler) mapToIp(chaddr net.HardwareAddr) net.IP {
	if len(self.baseIP) != 4 {
		panic("Unexpected baseIP length")
	}

	if len(chaddr) != 6 {
		panic("Unexpected chaddr length")
	}

	if len(self.baseHwaddr) != 6 {
		panic("Unexpected baseHwaddr length")
	}

	addr := make(net.IP, 4)
	copy(addr, self.baseIP)

	// Prefix must match
	for i := 0; i < 2; i++ {
		if self.baseHwaddr[i] != chaddr[i] {
			log.Println("MAC does not match base")
			return nil
		}
	}

	// Remainder must match
	for i := 2; i < 6; i++ {
		delta := self.baseHwaddr[i] ^ chaddr[i]
		if (delta&self.netmask[i-2]) != 0 {
			log.Println("MAC does not match netmask")
		}

		addr[i-2] = addr[i-2]|delta
	}

	log.Println("Mapped MAC", chaddr, "to", addr)

	return addr
}

func (self *DHCPHandler) ServeDHCP(p dhcp.Packet, msgType dhcp.MessageType, options dhcp.Options) (d dhcp.Packet) {
	switch msgType {

	case dhcp.Discover:
		log.Println("DHCP Discover from", p.CHAddr())

		ip := self.mapToIp(p.CHAddr())
		if ip == nil {
			return nil
		}
		return dhcp.ReplyPacket(p, dhcp.Offer, self.serverIP, ip, self.leaseDuration,
			self.options.SelectOrderOrAll(options[dhcp.OptionParameterRequestList]))

	case dhcp.Request:
		log.Println("DHCP Request from", p.CHAddr())

		if server, ok := options[dhcp.OptionServerIdentifier]; ok && !net.IP(server).Equal(self.serverIP) {
			return nil // Message not for this dhcp server
		}
		if reqIP := net.IP(options[dhcp.OptionRequestedIPAddress]); len(reqIP) == 4 {
			ip := self.mapToIp(p.CHAddr())
			if ip != nil {
				return dhcp.ReplyPacket(p, dhcp.ACK, self.serverIP, net.IP(options[dhcp.OptionRequestedIPAddress]), self.leaseDuration,
					self.options.SelectOrderOrAll(options[dhcp.OptionParameterRequestList]))
			}
		}
		return dhcp.ReplyPacket(p, dhcp.NAK, self.serverIP, nil, 0, nil)

	case dhcp.Release:
		log.Println("DHCP Release from", p.CHAddr())

	case dhcp.Decline:
		log.Println("DHCP Decline from", p.CHAddr())

	default:
		log.Println("DHCP message of type", msgType)

	}
	return nil
}
