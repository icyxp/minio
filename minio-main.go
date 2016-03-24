/*
 * Minio Cloud Storage, (C) 2015, 2016 Minio, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/minio/cli"
	"github.com/minio/mc/pkg/console"
	"github.com/minio/minio/pkg/fs"
	"github.com/minio/minio/pkg/minhttp"
	"github.com/minio/minio/pkg/probe"
)

var initCmd = cli.Command{
	Name:  "init",
	Usage: "Initialize Minio cloud storage server.",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:  "address",
			Value: ":9000",
		},
	},
	Action: initMain,
	CustomHelpTemplate: `NAME:
  minio {{.Name}} - {{.Usage}}

USAGE:
  minio {{.Name}} [OPTION VALUE] PATH

OPTIONS:
  {{range .Flags}}{{.}}
  {{end}}
ENVIRONMENT VARIABLES:
  MINIO_ACCESS_KEY, MINIO_SECRET_KEY: Access and secret key to use.

EXAMPLES:
  1. Start minio server on Linux.
      $ minio {{.Name}} fs /home/shared

  2. Start minio server on Windows.
      $ minio {{.Name}} fs C:\MyShare

  3. Start minio server bound to a specific IP:PORT, when you have multiple network interfaces.
      $ minio {{.Name}} --address 192.168.1.101:9000 fs /home/shared

  4. Start minio server with minimum free disk threshold to 5%
      $ minio {{.Name}} fs /home/shared/Pictures

`,
}

var serverCmd = cli.Command{
	Name:   "server",
	Usage:  "Start Minio cloud storage server.",
	Flags:  []cli.Flag{},
	Action: serverMain,
	CustomHelpTemplate: `NAME:
  minio {{.Name}} - {{.Usage}}

USAGE:
  minio {{.Name}}

EXAMPLES:
  1. Start minio server.
      $ minio {{.Name}}

`,
}

// configureServer configure a new server instance
func configureServer(filesystem fs.Filesystem) *http.Server {
	// Minio server config
	apiServer := &http.Server{
		Addr:           serverConfig.GetAddr(),
		Handler:        configureServerHandler(filesystem),
		MaxHeaderBytes: 1 << 20,
	}

	// Configure TLS if certs are available.
	if isSSL() {
		var e error
		apiServer.TLSConfig = &tls.Config{}
		apiServer.TLSConfig.Certificates = make([]tls.Certificate, 1)
		apiServer.TLSConfig.Certificates[0], e = tls.LoadX509KeyPair(mustGetCertFile(), mustGetKeyFile())
		fatalIf(probe.NewError(e), "Unable to load certificates.", nil)
	}

	// Returns configured HTTP server.
	return apiServer
}

// Print listen ips.
func printListenIPs(httpServerConf *http.Server) {
	host, port, e := net.SplitHostPort(httpServerConf.Addr)
	fatalIf(probe.NewError(e), "Unable to split host port.", nil)

	var hosts []string
	switch {
	case host != "":
		hosts = append(hosts, host)
	default:
		addrs, e := net.InterfaceAddrs()
		fatalIf(probe.NewError(e), "Unable to get interface address.", nil)
		for _, addr := range addrs {
			if addr.Network() == "ip+net" {
				host := strings.Split(addr.String(), "/")[0]
				if ip := net.ParseIP(host); ip.To4() != nil {
					hosts = append(hosts, host)
				}
			}
		}
	}
	for _, host := range hosts {
		if httpServerConf.TLSConfig != nil {
			console.Printf("    https://%s:%s\n", host, port)
		} else {
			console.Printf("    http://%s:%s\n", host, port)
		}
	}
}

// initServer initialize server
func initServer(c *cli.Context) {
	host, port, _ := net.SplitHostPort(c.String("address"))
	// If port empty, default to port '80'
	if port == "" {
		port = "80"
		// if SSL is enabled, choose port as "443" instead.
		if isSSL() {
			port = "443"
		}
	}

	// Join host and port.
	serverConfig.SetAddr(net.JoinHostPort(host, port))

	// Set backend FS type.
	if c.Args().Get(0) == "fs" {
		fsPath := strings.TrimSpace(c.Args().Get(1))
		// Last argument is always a file system path, verify if it exists and is accessible.
		_, e := os.Stat(fsPath)
		fatalIf(probe.NewError(e), "Unable to validate the path", nil)

		serverConfig.SetBackend(backend{
			Type: "fs",
			Disk: fsPath,
		})
	} // else { Add backend XL type here.

	// Fetch access keys from environment variables if any and update the config.
	accessKey := os.Getenv("MINIO_ACCESS_KEY")
	secretKey := os.Getenv("MINIO_SECRET_KEY")

	// Validate if both keys are specified and they are valid save them.
	if accessKey != "" && secretKey != "" {
		if !isValidAccessKey.MatchString(accessKey) {
			fatalIf(probe.NewError(errInvalidArgument), "Access key does not have required length", nil)
		}
		if !isValidSecretKey.MatchString(secretKey) {
			fatalIf(probe.NewError(errInvalidArgument), "Secret key does not have required length", nil)
		}
		serverConfig.SetCredential(credential{
			AccessKeyID:     accessKey,
			SecretAccessKey: secretKey,
		})
	}

	// Save new config.
	err := serverConfig.Save()
	fatalIf(err.Trace(), "Unable to save config.", nil)

	// Successfully written.
	backend := serverConfig.GetBackend()
	if backend.Type == "fs" {
		console.Println(colorGreen("Successfully initialized Minio at %s", backend.Disk))
	}
}

// check init arguments.
func checkInitSyntax(c *cli.Context) {
	if !c.Args().Present() || c.Args().First() == "help" {
		cli.ShowCommandHelpAndExit(c, "init", 1)
	}
	if len(c.Args()) > 2 {
		fatalIf(probe.NewError(errInvalidArgument), "Unnecessary arguments passed. Please refer ‘minio init --help’.", nil)
	}
	path := strings.TrimSpace(c.Args().Last())
	if path == "" {
		fatalIf(probe.NewError(errInvalidArgument), "Path argument cannot be empty.", nil)
	}
}

// extract port number from address.
// address should be of the form host:port
func getPort(address string) int {
	_, portStr, e := net.SplitHostPort(address)
	fatalIf(probe.NewError(e), "Unable to split host port.", nil)
	portInt, e := strconv.Atoi(portStr)
	fatalIf(probe.NewError(e), "Invalid port number.", nil)
	return portInt
}

// Make sure that none of the other processes are listening on the
// specified port on any of the interfaces.
//
// On linux if a process is listening on 127.0.0.1:9000 then Listen()
// on ":9000" fails with the error "port already in use".
// However on Mac OSX Listen() on ":9000" falls back to the IPv6 address.
// This causes confusion on Mac OSX that minio server is not reachable
// on 127.0.0.1 even though minio server is running. So before we start
// the minio server we make sure that the port is free on all the IPs.
func checkPortAvailability(port int) {
	isAddrInUse := func(e error) bool {
		// Check if the syscall error is EADDRINUSE.
		// EADDRINUSE is the system call error if another process is
		// already listening at the specified port.
		neterr, ok := e.(*net.OpError)
		if !ok {
			return false
		}
		osErr, ok := neterr.Err.(*os.SyscallError)
		if !ok {
			return false
		}
		sysErr, ok := osErr.Err.(syscall.Errno)
		if !ok {
			return false
		}
		if sysErr != syscall.EADDRINUSE {
			return false
		}
		return true
	}
	ifcs, e := net.Interfaces()
	if e != nil {
		fatalIf(probe.NewError(e), "Unable to list interfaces.", nil)
	}
	for _, ifc := range ifcs {
		addrs, e := ifc.Addrs()
		if e != nil {
			fatalIf(probe.NewError(e), fmt.Sprintf("Unable to list addresses on interface %s.", ifc.Name), nil)
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				errorIf(probe.NewError(errors.New("")), "Interface type assertion to (*net.IPNet) failed.", nil)
				continue
			}
			ip := ipnet.IP
			network := "tcp4"
			if ip.To4() == nil {
				network = "tcp6"
			}
			tcpAddr := net.TCPAddr{IP: ip, Port: port, Zone: ifc.Name}
			l, e := net.ListenTCP(network, &tcpAddr)
			if e != nil {
				if isAddrInUse(e) {
					// Fail if port is already in use.
					fatalIf(probe.NewError(e), fmt.Sprintf("Unable to listen on IP %s, port %.d", tcpAddr.IP, tcpAddr.Port), nil)
				} else {
					// Ignore other errors.
					continue
				}
			}
			e = l.Close()
			if e != nil {
				fatalIf(probe.NewError(e), fmt.Sprintf("Unable to close listener on IP %s, port %.d", tcpAddr.IP, tcpAddr.Port), nil)
			}
		}
	}
}

func initMain(c *cli.Context) {
	// check 'init' cli arguments.
	checkInitSyntax(c)

	// Initialize server.
	initServer(c)
}

func serverMain(c *cli.Context) {
	if c.Args().Present() || c.Args().First() == "help" {
		cli.ShowCommandHelpAndExit(c, "server", 1)
	}

	backend := serverConfig.GetBackend()
	if backend.Type == "fs" {
		// Initialize file system.
		filesystem, err := fs.New(backend.Disk)
		fatalIf(err.Trace(backend.Type, backend.Disk), "Initializing filesystem failed.", nil)

		// Configure server.
		apiServer := configureServer(filesystem)

		// Credential.
		cred := serverConfig.GetCredential()

		// Region.
		region := serverConfig.GetRegion()

		// Print credentials and region.
		console.Println("\n" + cred.String() + "  " + colorMagenta("Region: ") + colorWhite(region))

		console.Println("\nMinio Object Storage:")
		// Print api listen ips.
		printListenIPs(apiServer)

		console.Println("\nMinio Browser:")
		// Print browser listen ips.
		printListenIPs(apiServer)

		console.Println("\nTo configure Minio Client:")

		// Download 'mc' links.
		if runtime.GOOS == "windows" {
			console.Println("    Download 'mc' from https://dl.minio.io/client/mc/release/" + runtime.GOOS + "-" + runtime.GOARCH + "/mc.exe")
			console.Println("    $ mc.exe config host add myminio http://localhost:9000 " + cred.AccessKeyID + " " + cred.SecretAccessKey)
		} else {
			console.Println("    $ wget https://dl.minio.io/client/mc/release/" + runtime.GOOS + "-" + runtime.GOARCH + "/mc")
			console.Println("    $ chmod 755 mc")
			console.Println("    $ ./mc config host add myminio http://localhost:9000 " + cred.AccessKeyID + " " + cred.SecretAccessKey)
		}

		// Start server.
		err = minhttp.ListenAndServe(apiServer)
		errorIf(err.Trace(), "Failed to start the minio server.", nil)
	}
	console.Println(colorGreen("No known backends configured, please use ‘minio init --help’ to initialize a backend."))
}
