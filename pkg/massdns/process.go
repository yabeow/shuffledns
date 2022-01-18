package massdns

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/projectdiscovery/gologger"
	"github.com/projectdiscovery/shuffledns/internal/store"
	"github.com/projectdiscovery/shuffledns/pkg/parser"
	"github.com/remeh/sizedwaitgroup"
	"github.com/rs/xid"
)

// Process runs the actual enumeration process returning a file
func (c *Client) Process() error {
	// Process a created list or the massdns input
	inputFile := c.config.InputFile
	if c.config.MassdnsRaw != "" {
		inputFile = c.config.MassdnsRaw
	}

	// Check for blank input file or non-existent input file
	blank, err := IsBlankFile(inputFile)
	if err != nil {
		return err
	}
	if blank {
		return errors.New("blank input file specified")
	}

	// Create a store for storing ip metadata
	shstore := store.New()
	defer shstore.Close()

	// Set the correct target file
	massDNSOutput := filepath.Join(c.config.TempDir, xid.New().String())
	if c.config.MassdnsRaw != "" {
		massDNSOutput = c.config.MassdnsRaw
	}

	// Check if we need to run massdns
	if c.config.MassdnsRaw == "" {
		// Create a temporary file for the massdns output
		gologger.Info().Msgf("Creating temporary massdns output file: %s\n", massDNSOutput)
		err = c.runMassDNS(massDNSOutput, shstore)
		if err != nil {
			return fmt.Errorf("could not execute massdns: %w", err)
		}
	}

	gologger.Info().Msgf("Started parsing massdns output\n")

	err = c.parseMassDNSOutput(massDNSOutput, shstore)
	if err != nil {
		return fmt.Errorf("could not parse massdns output: %w", err)
	}

	gologger.Info().Msgf("Massdns output parsing compeleted\n")
	// Perform wildcard filtering only if domain name has been specified and --no-filter-wildcard is not set
	if !c.config.NoFilterWildcard && c.config.Domain != "" {
		gologger.Info().Msgf("Started removing wildcards records\n")
		err = c.filterWildcards(shstore)
		if err != nil {
			return fmt.Errorf("could not parse massdns output: %w", err)
		}
		gologger.Info().Msgf("Wildcard removal completed\n")
	}

	gologger.Info().Msgf("Finished enumeration, started writing output\n")

	// Write the final elaborated list out
	return c.writeOutput(shstore)
}

func (c *Client) runMassDNS(output string, store *store.Store) error {
	if c.config.Domain != "" {
		gologger.Info().Msgf("Executing massdns on %s\n", c.config.Domain)
	} else {
		gologger.Info().Msgf("Executing massdns\n")
	}
	now := time.Now()
	args := []string{"-r", c.config.ResolversFile, "-o", "Snl", "-t", "A", c.config.InputFile, "-w", output, "-s", strconv.Itoa(c.config.Threads)}
	if c.config.AllowRoot {
		args = append(args, "--root")
	}
	// Run the command on a temp file and wait for the output
	cmd := exec.Command(c.config.MassdnsPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("could not execute massdns: %w\ndetailed error: %s", err, stderr.String())
	}
	gologger.Info().Msgf("Massdns execution took %s\n", time.Since(now))
	return nil
}

func (c *Client) parseMassDNSOutput(output string, store *store.Store) error {
	massdnsOutput, err := os.Open(output)
	if err != nil {
		return fmt.Errorf("could not open massdns output file: %w", err)
	}
	defer massdnsOutput.Close()

	// at first we need the full structure in memory to elaborate it in parallell
	err = parser.Parse(massdnsOutput, func(domain string, ip []string) {
		for _, ip := range ip {
			// Check if ip exists in the store. If not,
			// add the ip to the map and continue with the next ip.
			if !store.Exists(ip) {
				store.New(ip, domain)
				continue
			}

			// Get the IP meta-information from the store.
			record := store.Get(ip)

			// Put the new hostname and increment the counter by 1.
			record.Hostnames[domain] = struct{}{}
			record.Counter++
		}
	})

	if err != nil {
		return fmt.Errorf("could not parse massdns output: %w", err)
	}

	return nil
}

func (c *Client) filterWildcards(st *store.Store) error {
	// Start to work in parallel on wildcards
	wildcardWg := sizedwaitgroup.New(c.config.WildcardsThreads)

	for _, record := range st.IP {
		// We've stumbled upon a wildcard, just ignore it.
		c.wildcardIPMutex.Lock()
		if _, ok := c.wildcardIPMap[record.IP]; ok {
			c.wildcardIPMutex.Unlock()
			continue
		}
		c.wildcardIPMutex.Unlock()

		// Perform wildcard detection on the ip, if an IP is found in the wildcard
		// we add it to the wildcard map so that further runs don't require such filtering again.
		if record.Counter >= 5 || c.config.StrictWildcard {
			wildcardWg.Add()
			go func(record *store.IPMeta) {
				defer wildcardWg.Done()

				for host := range record.Hostnames {
					isWildcard, ips := c.wildcardResolver.LookupHost(host)
					if len(ips) > 0 {
						c.wildcardIPMutex.Lock()
						for ip := range ips {
							// we add the single ip to the wildcard list
							c.wildcardIPMap[ip] = struct{}{}
						}
						c.wildcardIPMutex.Unlock()
					}

					if isWildcard {
						c.wildcardIPMutex.Lock()
						// we also mark the original ip as wildcard, since at least once it resolved to this host
						c.wildcardIPMap[record.IP] = struct{}{}
						c.wildcardIPMutex.Unlock()
						break
					}
				}
			}(record)
		}
	}

	wildcardWg.Wait()

	// drop all wildcard from the store
	for wildcardIP := range c.wildcardIPMap {
		st.Delete(wildcardIP)
	}

	return nil
}

func (c *Client) writeOutput(store *store.Store) error {
	// Write the unique deduplicated output to the file or stdout
	// depending on what the user has asked.
	var output *os.File
	var w *bufio.Writer
	var err error

	if c.config.OutputFile != "" {
		output, err = os.Create(c.config.OutputFile)
		if err != nil {
			return fmt.Errorf("could not create massdns output file: %v", err)
		}
		w = bufio.NewWriter(output)
	}
	buffer := &strings.Builder{}

	uniqueMap := make(map[string]struct{})

	for _, record := range store.IP {
		for hostname := range record.Hostnames {
			// Skip if we already printed this subdomain once
			if _, ok := uniqueMap[hostname]; ok {
				continue
			}
			uniqueMap[hostname] = struct{}{}

			if c.config.Json {
				hostnameJson, err := json.Marshal(map[string]interface{}{"hostname": hostname})
				if err != nil {
					return fmt.Errorf("could not marshal output as json: %v", err)
				}

				buffer.WriteString(string(hostnameJson))
				buffer.WriteString("\n")
			} else {
				buffer.WriteString(hostname)
				buffer.WriteString("\n")
			}

			data := buffer.String()

			if output != nil {
				_, _ = w.WriteString(data)
			}
			gologger.Silent().Msgf("%s", data)
			buffer.Reset()
		}
	}

	// Close the files and return
	if output != nil {
		w.Flush()
		output.Close()
	}
	return nil
}
