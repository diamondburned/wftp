package main

import "github.com/spf13/pflag"

var (
	listenAddr = ""
	secret     = ""
	daemon     = false
)

func init() {
	pflag.StringVarP(&listenAddr, "listen", "l", listenAddr, "listen address, if any")
	pflag.StringVarP(&secret, "secret", "s", secret, "secret passphrase to expect, optional")
	pflag.BoolVarP(&daemon, "daemon", "d", daemon, "run as daemon (no CLI)")
}
