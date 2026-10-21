package main

import (
	"log"

	"github.com/spf13/cobra"

	"paqet-plus/cmd/dump"
	"paqet-plus/cmd/ping"
	"paqet-plus/cmd/run"
	"paqet-plus/cmd/secret"
	"paqet-plus/cmd/version"
)

var rootCmd = &cobra.Command{
	Use:   "paqet-plus",
	Short: "KCP transport over raw TCP packet",
	Long:  `paqet-plus is a bidirectional packet-level proxy using KCP and raw socket transport with encryption.`,
}

func main() {
	rootCmd.AddCommand(run.Cmd)
	rootCmd.AddCommand(dump.Cmd)
	rootCmd.AddCommand(ping.Cmd)
	rootCmd.AddCommand(secret.Cmd)
	rootCmd.AddCommand(version.Cmd)

	if err := rootCmd.Execute(); err != nil {
		log.Fatalf("%v", err)
	}
}
