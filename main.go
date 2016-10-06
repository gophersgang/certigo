/*-
 * Copyright 2016 Square Inc.
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
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"strings"

	"gopkg.in/alecthomas/kingpin.v2"

	"github.com/square/certigo/lib"
	"github.com/square/certigo/pkcs7"
	"golang.org/x/crypto/ssh/terminal"
)

var (
	app = kingpin.New("certigo", "A command line certificate examination utility.")

	dump         = app.Command("dump", "Display information about a certificate from a file/stdin.")
	dumpFiles    = dump.Arg("file", "Certificate file to dump (or stdin if not specified).").ExistingFiles()
	dumpType     = dump.Flag("format", "Format of given input (PEM, DER, JCEKS, PKCS12; heuristic if missing).").String()
	dumpPem      = dump.Flag("pem", "Write output as PEM blocks instead of human-readable format.").Bool()
	dumpPassword = dump.Flag("password", "Password for PKCS12/JCEKS key stores (if required).").String()
	dumpJSON     = dump.Flag("json", "Write output as machine-readable JSON format.").Bool()

	connect       = app.Command("connect", "Connect to a server and print its certificate(s).")
	connectTo     = connect.Arg("server:port", "Hostname or IP to connect to.").String()
	connectName   = connect.Flag("name", "Override the server name used for Server Name Indication (SNI).").String()
	connectCaPath = connect.Flag("ca", "Path to CA bundle (system default if unspecified).").ExistingFile()
	connectPem    = connect.Flag("pem", "Write output as PEM blocks instead of human-readable format.").Bool()
	connectJSON   = connect.Flag("json", "Write output as machine-readable JSON format.").Bool()

	verify       = app.Command("verify", "Verify a certificate chain from file/stdin against a name.")
	verifyFile   = verify.Arg("file", "Certificate file to dump (or stdin if not specified).").ExistingFile()
	verifyName   = verify.Flag("name", "Server name to verify certificate against.").Required().String()
	verifyCaPath = verify.Flag("ca", "Path to CA bundle (system default if unspecified).").ExistingFile()
	verifyType   = dump.Flag("format", "Format of given input (PEM, DER, JCEKS, PKCS12; heuristic if missing).").String()
	verifyJSON   = verify.Flag("json", "Write output as machine-readable JSON format.").Bool()
)

func main() {
	app.Version("1.4.0")

	result := simpleResult{}
	switch kingpin.MustParse(app.Parse(os.Args[1:])) {
	case dump.FullCommand(): // Dump certificate
		files := inputFiles(*dumpFiles)
		defer func() {
			for _, file := range files {
				file.Close()
			}
		}()

		lib.ReadCerts(files, *dumpType, readPassword, func(block *pem.Block) {
			if *dumpPem {
				block.Headers = nil
				pem.Encode(os.Stdout, block)
				return
			}

			switch block.Type {
			case "CERTIFICATE":
				result.Certificates = append(result.Certificates, createSimpleCertificateFromX509(block))
			case "PKCS7":
				certs, err := pkcs7.ExtractCertificates(block.Bytes)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error parsing PKCS7 block: %s\n", err)
					os.Exit(1)
				}
				for _, cert := range certs {
					result.Certificates = append(result.Certificates, createSimpleCertificate(certWithName{cert: cert}))
				}
			}
		})

		if *dumpJSON {
			blob, _ := json.Marshal(result)
			fmt.Println(string(blob))
		} else {
			for i, cert := range result.Certificates {
				fmt.Printf("** CERTIFICATE %d **\n", i+1)
				displayCert(cert)
				fmt.Printf("\n\n")
			}
		}

	case connect.FullCommand(): // Get certs by connecting to a server
		conn, err := tls.Dial("tcp", *connectTo, &tls.Config{
			// We verify later manually so we can print results
			InsecureSkipVerify: true,
			ServerName:         *connectName,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error connecting: %v\n", err)
			os.Exit(1)
		}
		defer conn.Close()
		for _, cert := range conn.ConnectionState().PeerCertificates {
			if *connectPem {
				pem.Encode(os.Stdout, lib.CertToPem(cert, nil))
			} else {
				result.Certificates = append(result.Certificates, createSimpleCertificate(certWithName{cert: cert}))
			}
		}

		if !*connectPem {
			var hostname string
			if *connectName != "" {
				hostname = *connectName
			} else {
				hostname = strings.Split(*connectTo, ":")[0]
			}
			verifyResult := verifyChain(conn.ConnectionState().PeerCertificates, hostname, *connectCaPath)
			result.VerifyResult = &verifyResult
		}

		if *connectJSON {
			blob, _ := json.Marshal(result)
			fmt.Println(string(blob))
		} else if !*connectPem {
			for i, cert := range result.Certificates {
				fmt.Printf("** CERTIFICATE %d **\n", i+1)
				displayCert(cert)
				fmt.Print("\n\n")
			}
			printVerifyResult(*result.VerifyResult)
		}
	case verify.FullCommand():
		file := inputFile(*verifyFile)
		defer file.Close()

		chain := []*x509.Certificate{}
		lib.ReadCerts([]*os.File{file}, *verifyType, readPassword, func(block *pem.Block) {
			switch block.Type {
			case "CERTIFICATE":
				cert, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error reading cert: %s", err)
					os.Exit(1)
				}
				chain = append(chain, cert)
			case "PKCS7":
				certs, err := pkcs7.ExtractCertificates(block.Bytes)
				if err != nil {
					fmt.Fprintf(os.Stderr, "error parsing PKCS7 block: %s\n", err)
					os.Exit(1)
				}
				chain = append(chain, certs...)
			}
		})

		verifyResult := verifyChain(chain, *verifyName, *verifyCaPath)
		if *verifyJSON {
			blob, _ := json.Marshal(verifyResult)
			fmt.Println(string(blob))
		} else {
			printVerifyResult(verifyResult)
		}
		if verifyResult.Error != "" {
			os.Exit(1)
		}
	}
}

func inputFile(fileName string) *os.File {
	if fileName == "" {
		return os.Stdin
	}

	rawFile, err := os.Open(fileName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "unable to open file: %s\n", err)
		os.Exit(1)
	}
	return rawFile
}

func inputFiles(fileNames []string) []*os.File {
	files := []*os.File{}
	if fileNames != nil {
		for _, filename := range fileNames {
			rawFile, err := os.Open(filename)
			if err != nil {
				fmt.Fprintf(os.Stderr, "unable to open file: %s\n", err)
				os.Exit(1)
			}
			files = append(files, rawFile)
		}
	} else {
		files = append(files, os.Stdin)
	}
	return files
}

func readPassword(alias string) string {
	if *dumpPassword != "" {
		return *dumpPassword
	}

	var tty *os.File
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		tty = os.Stdin
	} else {
		defer tty.Close()
	}

	tty.WriteString("Enter password")
	if alias != "" {
		tty.WriteString(fmt.Sprintf(" for entry [%s]", alias))
	}
	tty.WriteString(": ")

	password, err := terminal.ReadPassword(int(tty.Fd()))
	tty.WriteString("\n")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error reading password: %s\n", err)
		os.Exit(1)
	}

	return strings.TrimSuffix(string(password), "\n")
}
