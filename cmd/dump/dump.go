package dump

import (
	"encoding/hex"
	"fmt"
	"os"
	"os/signal"
	"paqet/internal/flog"
	"strings"
	"time"

	"github.com/gopacket/gopacket"
	"github.com/gopacket/gopacket/layers"
	"github.com/gopacket/gopacket/pcap"
	"github.com/spf13/cobra"
)

var (
	iface   string
	port    int
	snaplen int
	promisc bool
)

func init() {
	Cmd.PersistentFlags().StringVarP(&iface, "interface", "i", "any", "Interface to listen on")
	Cmd.PersistentFlags().IntVarP(&port, "port", "p", 0, "TCP destination port to filter on")
	Cmd.PersistentFlags().IntVar(&snaplen, "snaplen", 65536, "Snapshot length for pcap")
	Cmd.PersistentFlags().BoolVar(&promisc, "promisc", true, "Set promiscuous mode")

	Cmd.MarkPersistentFlagRequired("port")
}

var Cmd = &cobra.Command{
	Use:   "dump",
	Short: "A raw packet dumper that logs TCP payloads for a given port.",
	Run: func(cmd *cobra.Command, args []string) {
		flog.Debugf("Starting packet listener on interface '%s' for TCP destination port %d...", iface, port)

		handle, err := pcap.OpenLive(iface, int32(snaplen), promisc, pcap.BlockForever)
		if err != nil {
			flog.Fatalf("Error opening pcap handle: %v", err)
		}
		filter := fmt.Sprintf("tcp and dst port %d", port)
		if err := handle.SetBPFFilter(filter); err != nil {
			flog.Fatalf("Error setting BPF filter '%s': %v", filter, err)
		}

		flog.Infof("Listener started. Waiting for packets... (Press Ctrl+C to exit)")

		packetSource := gopacket.NewPacketSource(handle, handle.LinkType())
		packets := packetSource.Packets()

		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt)
		for {
			select {
			case packet := <-packets:
				go handlePacket(packet)
			case <-sigChan:
				flog.Infof("Shutdown signal received, exiting.")
				return
			}
		}
	},
}

func handlePacket(packet gopacket.Packet) {
	appLayer := packet.ApplicationLayer()

	var payload []byte
	if appLayer != nil {
		payload = appLayer.Payload()
	}

	var srcAddr, dstAddr string
	if ipLayer := packet.Layer(layers.LayerTypeIPv4); ipLayer != nil {
		ip, _ := ipLayer.(*layers.IPv4)
		srcAddr = ip.SrcIP.String()
		dstAddr = ip.DstIP.String()
	}
	if tcpLayer := packet.Layer(layers.LayerTypeTCP); tcpLayer != nil {
		tcp, _ := tcpLayer.(*layers.TCP)
		srcAddr = fmt.Sprintf("%s:%s", srcAddr, tcp.SrcPort)
		dstAddr = fmt.Sprintf("%s:%s", dstAddr, tcp.DstPort)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf(
		"[%s] Packet: %s -> %s | Length: %d bytes\n",
		time.Now().Format("15:04:05.000"),
		srcAddr,
		dstAddr,
		len(payload),
	))
	sb.WriteString("--- PAYLOAD (HEX DUMP) ---\n")
	sb.WriteString(hex.Dump(payload))
	sb.WriteString("--------------------------")

	fmt.Println(sb.String())
}
