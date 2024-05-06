package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"syscall"
	"time"

	"github.com/coreos/go-iptables/iptables"
	"github.com/davecgh/go-spew/spew"
	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/nadoo/ipset"
	"gopkg.in/yaml.v3"
)

type PeerNode struct {
	Pubkey     solana.PublicKey
	GossipIp   string
	GossipPort string
	TPUIp      string
	TPUPort    string
	Stake      uint64
}

type trustedProviders []string

func (i *trustedProviders) String() string {
	return strings.Join(string, ",")
}

func (i *trustedProviders) Set(value string) error {
	*i = append(*i, value)
	return nil
}

var flagProviders trustedProviders = []string{}
var (
	flagConfigFile              = flag.String("config-file", "config.yml", "configuration file")
	flagPubkey                  = flag.String("pubkey", "", "validator-pubkey")
	flagRpcUri                  = flag.String("rpc-uri", "https://api.mainnet-beta.solana.com", "the rpc uri to use")
	flagRpcIdentity             = flag.Bool("fetch-identity", false, "fetch identity from rpc")
	flagOurLocalhost            = flag.Bool("our-localhost", false, "use localhost:8899 for rpc and fetch identity from that rpc")
	flagDefaultTPUPolicy        = flag.String("tpu-policy", "", "the default iptables policy for tpu, default is passthrough")
	flagDefaultTPUQuicPolicy    = flag.String("tpu-quic-policy", "", "the default iptables policy for quic tpu, default is passthrough")
	flagDefaultTPUQuicfwdPolicy = flag.String("tpu-quic-fwd-policy", "", "the default iptables policy for quic tpu fwd, default is passthrough")
	flagDefaultFWDPolicy        = flag.String("fwd-policy", "", "the default iptables policy for tpu forward, default is passthrough")
	flagDefaultVotePolicy       = flag.String("vote-policy", "", "the default iptables policy for votes, default is passthrough")
	flagUpdateIpSets            = flag.Bool("update", true, "whether or not to keep ipsets updated")
	flagSleep                   = flag.Duration("sleep", 10*time.Second, "how long to sleep between updates")

	mangleChain       = "solana-nodes"
	filterChain       = "solana-tpu"
	filterChainCustom = "solana-tpu-custom"
	gossipSet         = "solana-gossip"

	quit = make(chan os.Signal)
)

type TrafficClass struct {
	Name   string  `yaml:"name"`
	Stake  float64 `yaml:"stake_percentage"` // If it has more than this stake
	FwMark uint64  `yaml:"fwmark,omitempty"`
}

type CustomNode struct {
	Name string `yaml:"name"`
	Ip   string `yaml:"ip"`
}

type TPConfig struct {
	Nodes []CustomNode `yaml:"nodes"`
}

type Config struct {
	Classes         []TrafficClass `yaml:"staked_classes"`
	UnstakedClass   TrafficClass   `yaml:"unstaked_class"`
	CustomNodeClass TrafficClass   `yaml:"custom_node_class"`
	CustomNodes     []CustomNode   `yaml:"custom_node_entries"`
}

func createChain(ipt *iptables.IPTables, table string, filterChain string, policy string) {
	exists, err := ipt.ChainExists(table, filterChain)
	if err != nil {
		log.Println("couldn't check existence", filterChain, err)
		os.Exit(1)
	}
	if !exists {
		err = ipt.NewChain(table, filterChain)
		if err != nil {
			log.Println("couldn't create filter chain", filterChain, err)
			os.Exit(1)
		}
	}
	if policy != "" {
		// Append the policy to the filter chain if it is specified
		err = ipt.AppendUnique(table, filterChain, "-j", policy)
		if err != nil {
			log.Println("couldn't set policy", policy, " on ", filterChain, err)
		}
	}
}

func deleteMangleInputRules(ipt *iptables.IPTables, port, mangleChain, filterChain string) {
	ipt.Delete("mangle", "PREROUTING", "-p", "udp", "--dport", port, "-j", mangleChain)
	ipt.Delete("filter", "INPUT", "-p", "udp", "--dport", port, "-j", filterChain)
}

func insertMangleInputRules(ipt *iptables.IPTables, port, mangleChain, filterChain string) {
	err := ipt.AppendUnique("mangle", "PREROUTING", "-p", "udp", "--dport", port, "-j", mangleChain)
	if err != nil {
		log.Println("couldn't add mangle rule for port", port, err)
	}

	exists, err := ipt.Exists("filter", "INPUT", "-p", "udp", "--dport", port, "-j", filterChain)
	if err != nil {
		log.Println("couldn't add filter rule for port", port, err)
	}

	if !exists {
		err = ipt.Insert("filter", "INPUT", 1, "-p", "udp", "--dport", port, "-j", filterChain)
		if err != nil {
			log.Println("couldn't add filter rule for port", port, err)
		}
	}
}

func cleanUp(c <-chan os.Signal, cfg *Config, ipt *iptables.IPTables, validatorPorts *ValidatorPorts) {
	<-c

	log.Println("Cleaning up and deleting all sets and firewall rules")

	// Clean up
	ipset.Flush(gossipSet)
	ipset.Destroy(gossipSet)

	for _, set := range cfg.Classes {
		ipset.Flush(set.Name)
		ipset.Destroy(set.Name)
		//ipt.Delete("mangle", mangleChain, "-m", "set", "--match-set", set.Name, "src", "-j", "MARK", "--set-mark", "4")
	}

	// We didn't find the TPU port so we never added those rules
	if validatorPorts != nil {
		deleteMangleInputRules(ipt, validatorPorts.TPUstr(), mangleChain, filterChain)
		deleteMangleInputRules(ipt, validatorPorts.Fwdstr(), mangleChain, filterChain+"-fwd")
		deleteMangleInputRules(ipt, validatorPorts.Votestr(), mangleChain, filterChain+"-vote")
		deleteMangleInputRules(ipt, validatorPorts.TPUquicstr(), mangleChain, filterChain+"-quic")
		deleteMangleInputRules(ipt, validatorPorts.TPUquicfwdstr(), mangleChain, filterChain+"-quic-fwd")
	}

	// Just in case, clean these rules up
	ipt.Delete("mangle", "PREROUTING", "-p", "udp", "-j", mangleChain)
	ipt.Delete("filter", "INPUT", "-p", "udp", "-j", filterChain)
	ipt.Delete("filter", "INPUT", "-p", "udp", "-j", filterChain+"-fwd")
	ipt.Delete("filter", "INPUT", "-p", "udp", "-j", filterChain+"-vote")
	ipt.Delete("filter", "INPUT", "-p", "udp", "-j", filterChain+"-quic")
	ipt.Delete("filter", "INPUT", "-p", "udp", "-j", filterChain+"-quic-fwd")

	// Clear and delete these chains
	ipt.ClearAndDeleteChain("mangle", mangleChain)
	ipt.ClearAndDeleteChain("filter", filterChain)
	ipt.ClearAndDeleteChain("filter", filterChain+"-fwd")
	ipt.ClearAndDeleteChain("filter", filterChain+"-vote")
	ipt.ClearAndDeleteChain("filter", filterChain+"-quic")
	ipt.ClearAndDeleteChain("filter", filterChain+"-quic-fwd")

	// Only delete the custom chain if it is empty
	ipt.DeleteChain("filter", filterChainCustom)
	ipt.DeleteChain("filter", filterChainCustom+"-fwd")
	ipt.DeleteChain("filter", filterChainCustom+"-vote")
	ipt.DeleteChain("filter", filterChainCustom+"-quic")
	ipt.DeleteChain("filter", filterChainCustom+"-quic-fwd")

	log.Println("Finished cleaning up")

	os.Exit(1)
}

func reloadConfig(c <-chan os.Signal, cfg *Config) {
	<-c

	log.Println("Reloading configuration files")

	// @TODO reload configuration file
}

func setUpdate(c <-chan os.Signal) {
	<-c

	log.Println("Updating ipsets")
	// @TODO change the ipset
}

func main() {
	flag.Var(&flagProviders, "trusted-providers", "[repeated] files for custom nodes. ex: -trusted-providers ./trusted_providers/helius.yml")
	flag.Parse()

	// Set validator ports to nil to start with
	var validatorPorts *ValidatorPorts = nil

	// Load traffic classes
	f, err := os.Open(*flagConfigFile)
	if err != nil {
		log.Println("couldn't open config file", *flagConfigFile, err)
		os.Exit(1)
	}

	if *flagOurLocalhost {
		*flagRpcUri = "http://localhost:8899/"
		*flagRpcIdentity = true
	}

	// Load config file
	var cfg Config
	decoder := yaml.NewDecoder(f)
	err = decoder.Decode(&cfg)
	if err != nil {
		log.Println("couldn't decode config file", *flagConfigFile, err)
		os.Exit(1)
	}

	for _, tp_file := range flagProviders {
		f, err := os.Open(tp_file)
		if err != nil {
			log.Println("couldn't open config file", *flagConfigFile, err)
			os.Exit(1)
		}
		var conf TPConfig
		dec := yaml.NewDecoder(f)
		err = dec.Decode(&conf)
		if err != nil {
			log.Println("couldn't decode trusted provider config file", *flagConfigFile, err)
			os.Exit(1)
		}

		for _, node := range conf.Nodes {
			cfg.CustomNodes = append(cfg.CustomNodes, node)
		}
	}

	// Special traffic class for unstaked nodes visible in gossip (e.g. RPC)
	cfg.UnstakedClass.Stake = -1
	cfg.Classes = append(cfg.Classes, cfg.UnstakedClass)

	cfg.CustomNodeClass.Stake = 100
	cfg.Classes = append(cfg.Classes, cfg.CustomNodeClass)
	// Sort the classes by stake weight
	sort.SliceStable(cfg.Classes, func(i, j int) bool {
		return cfg.Classes[i].Stake > cfg.Classes[j].Stake
	})

	// Connect to rpc
	client := rpc.New(*flagRpcUri)

	// Fetch identity
	if *flagRpcIdentity {
		out, err := client.GetIdentity(context.TODO())
		if err == nil {
			*flagPubkey = out.Identity.String()
			log.Println("loaded identity=", *flagPubkey)
		} else {
			log.Println("couldn't fetch validator identity, firewall will not by default handle tpu/tpufwd/vote ports", err)
		}
	}

	// Create iptables and ipset
	ipt, err := iptables.New()
	if err != nil {
		log.Println("couldn't init iptables", err)
		os.Exit(1)
	}

	if err := ipset.Init(); err != nil {
		log.Println("error in ipset init", err)
		os.Exit(1)
	}

	// Clear the ipsets
	ipset.Create(gossipSet)
	ipset.Flush(gossipSet)
	for _, set := range cfg.Classes {
		ipset.Create(set.Name)
		ipset.Flush(set.Name)
	}

	// Clean up on signals
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM, syscall.SIGINT)
	go cleanUp(c, &cfg, ipt, validatorPorts)

	cUSR1 := make(chan os.Signal, 1)
	signal.Notify(cUSR1, syscall.SIGUSR1)
	go setUpdate(cUSR1)

	cHUP := make(chan os.Signal, 1)
	signal.Notify(cHUP, syscall.SIGHUP)
	go reloadConfig(cHUP, &cfg)

	// Add base rules for marking packets in iptables
	createChain(ipt, "mangle", mangleChain, "ACCEPT")
	createChain(ipt, "filter", filterChain, *flagDefaultTPUPolicy)
	createChain(ipt, "filter", filterChain+"-fwd", *flagDefaultFWDPolicy)
	createChain(ipt, "filter", filterChain+"-vote", *flagDefaultVotePolicy)
	createChain(ipt, "filter", filterChain+"-quic", *flagDefaultTPUQuicPolicy)
	createChain(ipt, "filter", filterChain+"-quic-fwd", *flagDefaultTPUQuicfwdPolicy)

	// No need to use default policies on the custom chains as they'll fall through to the other chains
	createChain(ipt, "filter", filterChainCustom, "")
	createChain(ipt, "filter", filterChainCustom+"-fwd", "")
	createChain(ipt, "filter", filterChainCustom+"-vote", "")
	createChain(ipt, "filter", filterChainCustom+"-quic", "")
	createChain(ipt, "filter", filterChainCustom+"-quic-fwd", "")

	// Create mangle rules for all the classes
	for _, set := range cfg.Classes {
		err = ipt.AppendUnique("mangle", mangleChain, "-m", "set", "--match-set", set.Name, "src", "-j", "MARK", "--set-mark", strconv.FormatUint(set.FwMark, 10))
		if err != nil {
			log.Println("couldn't append mangle rule for set ", set.Name, err)
		}
	}

	// If there's no pubkey, then send all matching traffic to the filter chain and the mangle chain
	if flagPubkey == nil || *flagPubkey == "" {
		err := ipt.AppendUnique("mangle", "PREROUTING", "-p", "udp", "-j", mangleChain)
		if err != nil {
			log.Println("could not add prerouting mangle chain: ", err)
		}
		/*@TODO: what to do in this default scenario? Perhaps create rules only from nodes in gossip?
		err = ipt.AppendUnique("filter", "INPUT", "-p", "udp", "-j", filterChain)
		if err != nil {
			log.Println("could not add input filter chain: ", err)
		}*/
	}

	// Add the forwarding rules from the main filter chain to the custom rules one
	err = ipt.Insert("filter", filterChain, 1, "-j", filterChainCustom)
	if err != nil {
		log.Println("could not add custom rules chain: ", err)
	}
	err = ipt.Insert("filter", filterChain+"-fwd", 1, "-j", filterChainCustom+"-fwd")
	if err != nil {
		log.Println("could not add custom rules chain: ", err)
	}
	err = ipt.Insert("filter", filterChain+"-vote", 1, "-j", filterChainCustom+"-vote")
	if err != nil {
		log.Println("could not add custom rules chain: ", err)
	}

	err = ipt.Insert("filter", filterChain+"-quic", 1, "-j", filterChainCustom+"-quic")
	if err != nil {
		log.Println("could not add custom rules chain: ", err)
	}

	err = ipt.Insert("filter", filterChain+"-quic-fwd", 1, "-j", filterChainCustom+"-quic-fwd")
	if err != nil {
		log.Println("could not add custom rules chain: ", err)
	}

	//Custom nodes are static, no need to retrieve them from gossip
	for _, node := range cfg.CustomNodes {
		log.Println("Adding custom node: ", node.Name, " with IP: ", node.Ip, " to set: ", cfg.CustomNodeClass.Name)
		err := ipset.Add(gossipSet, node.Ip)

		if err != nil {
			log.Println("Failed to add node to gossip set: ", err)
		}
		cn := ipset.Add(cfg.CustomNodeClass.Name, node.Ip)
		if cn != nil {
			log.Println("Failed to add node to set", cfg.CustomNodeClass.Name, err)
		}

	}

	for {
		var totalStake uint64 = 0
		var stakedPeers map[string]PeerNode

		// No need to update ipsets
		if *flagUpdateIpSets {
			log.Println("Updating stake weights")

			stakedNodes, err := client.GetVoteAccounts(
				context.TODO(),
				&rpc.GetVoteAccountsOpts{},
			)

			if err != nil {
				log.Println("couldn't load vote accounts nodes", err)
				time.Sleep(time.Second * 5)
				continue
			}

			// Current nodes
			stakedPeers = make(map[string]PeerNode)

			for _, node := range stakedNodes.Current {
				totalStake += node.ActivatedStake

				// Don't add my self and don't add unstaked nodes
				if *flagPubkey != "" || flagPubkey != nil {
					if node.NodePubkey.String() == *flagPubkey {
						continue
					}
				}
				if node.ActivatedStake <= 0 {
					continue
				}

				stakedPeers[node.NodePubkey.String()] = PeerNode{
					Stake:  node.ActivatedStake,
					Pubkey: node.NodePubkey,
				}
			}

			// Delinquent nodes
			for _, node := range stakedNodes.Delinquent {
				totalStake += node.ActivatedStake

				// Don't add my self and don't add unstaked nodes
				if *flagPubkey != "" || flagPubkey != nil {
					if node.NodePubkey.String() == *flagPubkey {
						continue
					}
				}
				if node.ActivatedStake <= 0 {
					continue
				}

				stakedPeers[node.NodePubkey.String()] = PeerNode{
					Stake:  node.ActivatedStake,
					Pubkey: node.NodePubkey,
				}
			}
		}

		// Fetch the IPs for all the cluster nodes
		nodes, err := client.GetClusterNodes(
			context.TODO(),
		)

		if err != nil {
			log.Println("couldn't load cluster nodes", err)
			time.Sleep(time.Second * 5)
			continue
		}

		// @TODO if a node disappears from gossip, it would be good to remove it from the ipset
		// otherwise the ipsets will just continue to grow, samething for our own tpu host address
		// if we change IP.
		for _, node := range nodes {
			if *flagPubkey != "" {
				if *flagPubkey == node.Pubkey.String() {
					spew.Dump(node)
					// If this is our node, configure the TPU forwarding rules
					if node.TPU != nil {
						tpuAddr := *node.TPU
						quicTPUAddr := *node.TPUQUIC
						_, tpu_port, err := net.SplitHostPort(tpuAddr)
						_, tpu_quic_port, errq := net.SplitHostPort(quicTPUAddr)
						if err == nil && errq == nil {
							port, err := strconv.Atoi(tpu_port)
							quic_port, errq := strconv.Atoi(tpu_quic_port)
							if err == nil && errq == nil {
								if validatorPorts != nil {
									if validatorPorts.TPU != uint16(port) ||
										validatorPorts.TPUquic != uint16(quic_port) {
										// TPU has changed, clean up before re-adding
										deleteMangleInputRules(ipt, validatorPorts.TPUstr(), mangleChain, filterChain)
										deleteMangleInputRules(ipt, validatorPorts.Fwdstr(), mangleChain, filterChain+"-fwd")
										deleteMangleInputRules(ipt, validatorPorts.Votestr(), mangleChain, filterChain+"-vote")
										deleteMangleInputRules(ipt, validatorPorts.TPUquicstr(), mangleChain, filterChain+"-quic")
										deleteMangleInputRules(ipt, validatorPorts.TPUquicfwdstr(), mangleChain, filterChain+"-quic-fwd")
									}
								}
								validatorPorts = NewValidatorPorts(uint16(port), uint16(quic_port))

								insertMangleInputRules(ipt, validatorPorts.TPUstr(), mangleChain, filterChain)
								insertMangleInputRules(ipt, validatorPorts.Fwdstr(), mangleChain, filterChain+"-fwd")
								insertMangleInputRules(ipt, validatorPorts.Votestr(), mangleChain, filterChain+"-vote")
								insertMangleInputRules(ipt, validatorPorts.TPUquicstr(), mangleChain, filterChain+"-quic")
								insertMangleInputRules(ipt, validatorPorts.TPUquicfwdstr(), mangleChain, filterChain+"-quic-fwd")

								log.Println("validator ports set, identity=", *flagPubkey, " tpu=", validatorPorts.TPUstr(), "tpufwd=", validatorPorts.Fwdstr(), "vote=", validatorPorts.Votestr(), "quic=", validatorPorts.TPUquicstr(), "quic-fwd=", validatorPorts.TPUquicfwdstr())

								if !(*flagUpdateIpSets) {
									// we've found our validator, let's not look at any other nodes
									break
								}
							} else {
								log.Println("couldn't load validator ports for your pubkey", err)
							}
						} else {
							log.Println("error parsing your validator ports", err)
						}
					}
				}
			}

			// If the node has a gossip address
			if node.Gossip != nil {
				// Currently add both TPU and Gossip addresses if both are listed
				// not sure if TPU would ever be different from gossip (assumption: not)
				var addresses []string
				gossip_host, _, err := net.SplitHostPort(*(node.Gossip))
				if err != nil {
					spew.Dump(node.Gossip)
					log.Println("couldn't parse gossip host", *(node.Gossip), err)
					continue
				}
				addresses = append(addresses, gossip_host)

				if node.TPU != nil {
					tpu := *(node.TPU)
					if tpu != "" {
						tpu_host, _, err := net.SplitHostPort(tpu)
						if err == nil {
							if tpu_host != gossip_host {
								addresses = append(addresses, tpu_host)
							}
						} else {
							log.Println("couldn't parse tpu host", err)
						}
					}
				}

				// If this is a staked node i.e. listed in staked peers
				if val, ok := stakedPeers[node.Pubkey.String()]; ok {
					percent := float64(val.Stake) / float64(totalStake)

					added := false
					for _, class := range cfg.Classes {
						// Add to the highest class it matches
						for _, addr := range addresses[1:] {
							ipset.Add(gossipSet, addr) // add all addresses to the gossipset

							if percent > class.Stake && !added {
								// Add to the first class found, then set flag
								// so we don't add it to any lower staked classes
								ipset.Add(class.Name, addr)
								added = true
							} else {
								// Delete from all other clasess
								ipset.Del(class.Name, addr)
							}
						}
					}
				} else {
					// unstaked node add to the special unstaked class
					// and delete from all other classes
					for _, addr := range addresses {
						ipset.Add(gossipSet, addr) // add all addresses to the gossipset
						ipset.Add(cfg.UnstakedClass.Name, addr)
						for _, class := range cfg.Classes[1:] {
							if class.Name != cfg.UnstakedClass.Name {
								ipset.Del(class.Name, addr)
							}
						}
					}
				}
			} else {
				fmt.Println("not visible in gossip", node.Pubkey.String())
			}
		}
		if *flagUpdateIpSets {
			log.Println("updated ipsets: ", len(nodes), " visible in gossip and added to ipset")
		} else {
			log.Println("not updating ipsets")
		}

		// update every 10 secs
		time.Sleep(*flagSleep)
	}
}
