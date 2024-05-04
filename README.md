# TPU traffic classifier

**Use at your own risk: While this is tested to work well, it's early stage software used for testing and experiments.**

This small program classifies incoming Solana network traffic and creates ipsets and iptables firewall rules. It can be used by validators to restrict access to the TPU & TPU-Forward ports according to the class of traffic.

By default, it listens to Gossip and creates/maintains the following classes of ipsets:

 - `solana-gossip`: all ips visible in gossip
 - `solana-unstaked`: unstaked nodes visible in gossip
 - `solana-staked`: staked nodes visible in gossip
 - `solana-min-staked`: nodes visible in gossip with minimal stake
 - `solana-high-staked`: nodes visible in gossip with significant stake
 - `custom-nodes` : custom peer nodes

These sets will be kept up to date for as long as the software runs. On exit it will clean up the sets.

You can modify these categories by editing `config.yml`, setting the minimum stake percentages for each category. The sender will be placed in the largest category that applies to it.

It also uses the PREROUTING tables to permanently mark traffic from these sets of IPs on the local nodes . This can be used in later traffic rules. By default the following fwmarks are set:

 - `1`: unstaked
 - `3`: staked
 - `5`: min staked > 15K
 - `9`: high staked > 150K
 - `11`: custom nodes

If you provide you validator pubkey it will assume that your validator runs on localhost and it will lookup the TPU port of the validator and enable the firewalling rules. If you do not provide your validator pubkey, all UDP traffic passing through this host will be passed through the chains created by this tool.

##  Running

Run: `go run .`

Build: `go build -o tpu-traffic-classifier .`

```
Usage of ./tpu-traffic-classifier:
  -config-file string
    	configuration file (default "config.yml")
  -fetch-identity
    	fetch identity from rpc
  -fwd-policy string
    	the default iptables policy for tpu forward, default is passthrough
  -our-localhost
    	use localhost:8899 for rpc and fetch identity from that rpc
  -pubkey string
    	validator-pubkey
  -rpc-uri string
    	the rpc uri to use (default "https://api.mainnet-beta.solana.com")
  -sleep duration
    	how long to sleep between updates (default 10s)
  -tpu-policy string
    	the default iptables policy for tpu, default is passthrough
  -tpu-quic-fwd-policy string
    	the default iptables policy for quic tpu fwd, default is passthrough
  -tpu-quic-policy string
    	the default iptables policy for quic tpu, default is passthrough
  -update
    	whether or not to keep ipsets updated (default true)
  -vote-policy string
    	the default iptables policy for votes, default is passthrough
  -trusted-providers [repeated] string
     files to manage lists of custom nodes
```

## Sample config.yml

```
# Special unstaked class for all nodes visible in gossip but without stake
unstaked_class:
  name: solana-unstaked
  fwmark: 1

# Different staked classes, the highest matching class will apply
staked_classes:
  - name: solana-staked
    stake_percentage: 0
    fwmark: 3
  - name: solana-min-staked
    stake_percentage: 0.00003 # 15k stake and up - 0.003%
    fwmark: 5
  - name: solana-high-staked 
    stake_percentage: 0.0003 # 150k stake and up - 0.03%
    fwmark: 9
    
# Custom nodes class, i.e. an allowlist for nodes not in gossip
custom_node_class:
  name: custom-nodes
  fwmark: 11

custom_node_entries:
  - name: my_rpc_node
    ip: 1.2.3.4
  - name: my_other_node
    ip: 4.5.6.7
```

## Sample Custom Providers file

```
nodes:
  -name: my_node
   ip: 1.2.3.4
  -name: my_other_node
   ip: 4.5.6.7
```

## Firewalling

**If you do not provide a validator pubkey, then all UDP traffic will pass through these firewall rules**.

You can add rules to `solana-tpu-custom` (or `solana-tpu-custom-quic`, `solana-tpu-custom-quic-fwd`). This chain will persist between invocations of this tool (it's not cleaned out). If you provide your validator pubkey, then the tool will look up your TPU port and send all incoming UDP TPU traffic to this port to the `solana-tpu-custom` chain. Same for the quic and quic-fwd ports.

For instance if you wanted to temporarily close TPU ports you can run:

```
iptables -A solana-tpu-custom -j DROP
```
This will drop all traffic to the tpu port.

If you would like to drop all traffic to UDP TPU port but allow UDP TPU and QUIC forwards from staked validators and allow all QUIC connections except from nodes in gossip:

```
# Old UDP TPU
iptables -N solana-tpu-custom || true
iptables -F solana-tpu-custom
iptables -A solana-tpu-custom -m set --match-set solana-high-staked src -j DROP
iptables -A solana-tpu-custom -m set --match-set solana-min-staked src -j DROP
iptables -A solana-tpu-custom -m set --match-set solana-staked src -j DROP
iptables -A solana-tpu-custom -m set --match-set solana-unstaked src -j DROP
iptables -A solana-tpu-custom -m set ! --match-set solana-gossip src -j DROP
iptables -A solana-tpu-custom -j DROP
# Old UDP TPU Forwards
iptables -N solana-tpu-custom-fwd || true
iptables -F solana-tpu-custom-fwd
iptables -A solana-tpu-custom-fwd -m set --match-set custom-nodes src -j ACCEPT
iptables -A solana-tpu-custom-fwd -m set --match-set solana-high-staked src -j ACCEPT
iptables -A solana-tpu-custom-fwd -m set --match-set solana-min-staked src -j ACCEPT
iptables -A solana-tpu-custom-fwd -m set --match-set solana-staked src -j ACCEPT
iptables -A solana-tpu-custom-fwd -m set --match-set solana-unstaked src -j DROP
iptables -A solana-tpu-custom-fwd -m set --match-set solana-gossip src -j DROP
iptables -A solana-tpu-custom-fwd -m set ! --match-set solana-gossip src -j DROP
iptables -A solana-tpu-custom-fwd -j DROP
# New QUIC TPU
iptables -N solana-tpu-custom-quic || true
iptables -F solana-tpu-custom-quic
iptables -A solana-tpu-custom-quic -m set --match-set custom-nodes src -j ACCEPT
iptables -A solana-tpu-custom-quic -m set --match-set solana-high-staked src -j ACCEPT
iptables -A solana-tpu-custom-quic -m set --match-set solana-min-staked src -j ACCEPT
iptables -A solana-tpu-custom-quic -m set --match-set solana-staked src -j ACCEPT
iptables -A solana-tpu-custom-quic -m set --match-set solana-unstaked src -j ACCEPT
iptables -A solana-tpu-custom-quic -m set ! --match-set solana-gossip src -j DROP # this will drop all QUIC connections from nodes not in gossip
iptables -A solana-tpu-custom-quic -j DROP
# New QUIC TPU Forwards
iptables -N solana-tpu-custom-quic-fwd || true
iptables -F solana-tpu-custom-quic-fwd
iptables -A solana-tpu-custom-quic-fwd -m set --match-set custom-nodes src -j ACCEPT
iptables -A solana-tpu-custom-quic-fwd -m set --match-set solana-high-staked src -j ACCEPT
iptables -A solana-tpu-custom-quic-fwd -m set --match-set solana-min-staked src -j ACCEPT
iptables -A solana-tpu-custom-quic-fwd -m set --match-set solana-staked src -j DROP
iptables -A solana-tpu-custom-quic-fwd -m set --match-set solana-unstaked src -j DROP
iptables -A solana-tpu-custom-quic-fwd -m set ! --match-set solana-gossip src -j DROP
iptables -A solana-tpu-custom-quic-fwd -j DROP
```

If you would only allow nodes in gossip to send to your TPU:

```
iptables -A solana-tpu-custom-quic -m set --match-set solana-gossip src -j ACCEPT
iptables -A solana-tpu-custom-quic -j DROP
```

Log all traffic from nodes not in gossip to you QUIC TPU fwd:

```
iptables -A solana-tpu-custom-quic-fwd -m set ! --match-set solana-gossip src -j LOG --log-prefix 'TPUfwd:not in gossip:' --log-level info
```

These rules will only work when this utility is running. When it is not running, the TPU port will be open as usual.

## Rate limiting example

You can rate limit traffic to reduce the load on your TPU port:

```
#!/bin/bash

iptables -F solana-tpu-custom
# accept any amount of traffic from nodes with more than 100k stake:
iptables -A solana-tpu-custom -m set --match-set solana-high-staked src -j ACCEPT  
# accept 50/udp/second from low staked nodes
iptables -A solana-tpu-custom -m set --match-set solana-staked src -m limit --limit 50/sec -j ACCEPT                
# accept 1000 packets/second from RPC nodes (and other unstaked)
iptables -A solana-tpu-custom -m set --match-set solana-unstaked src -m limit --limit 1000/sec  -j ACCEPT # rpc nodes   
# accept 10 packets/second from nodes not visible in gossip
iptables -A solana-tpu-custom -m set ! --match-set solana-gossip src -m limit --limit 10/sec -j ACCEPT       
# log all dropped traffic (warning: lots of logs)
iptables -A solana-tpu-custom -j LOG --log-prefix "TPUport:" --log-level info
# drop everything that doesn't pass the limit
iptables -A solana-tpu-custom -j DROP

iptables -F solana-tpu-custom-fwd
# accept only forwarding traffic from nodes in gossip:
iptables -A solana-tpu-custom-fwd -m set --match-set solana-gossip src -j ACCEPT                                                                             
iptables -A solana-tpu-custom-fwd -j LOG --log-prefix "TPUfwd:" --log-level info                                                                             
iptables -A solana-tpu-custom-fwd -j DROP
```

## Traffic shaping

**Incomplete example, not usable as-is**

You can use the fwmarks set by this tool to create traffic classes for QoS/traffic shaping. You need to use IFB for incoming traffic filteringtraffic . 


```
tc qdisc add dev eth0 handle 1: ingress

tc filter add dev eth0 protocol ip parent 1: prio 1 handle 1 fw flowid 1:10 police rate 100mbit burst 100k # unstaked
tc filter add dev eth0 protocol ip parent 1: prio 1 handle 3 fw flowid 1:20 # staked
tc filter add dev eth0 protocol ip parent 1: prio 1 handle 9 fw flowid 1:30 # high staked
tc filter add dev eth0 protocol ip parent 1: prio 1 handle 6 fw flowid 1:40 # others
```


## Example iptables generated

The examples below is generated by this tool when run with the `pubkey` param for a valid validator. When the tool exits it will clean these rules up with the exception of `-custom...`  if (and only if) it's not empty.

```
*filter
:INPUT ACCEPT [0:0]
:FORWARD DROP [0:0]
:OUTPUT ACCEPT [0:0]
:solana-tpu - [0:0]
:solana-tpu-custom - [0:0]
-A INPUT -p udp -m udp --dport 8004 -j solana-tpu
-A INPUT -p udp -m udp --dport 8005 -j solana-tpu-fwd
-A INPUT -p udp -m udp --dport 8006 -j solana-tpu-vote
-A solana-tpu -j solana-tpu-custom
-A solana-tpu-fwd -j solana-tpu-custom-fwd
-A solana-tpu-vote -j solana-tpu-custom-vote
COMMIT
```

```
*mangle
:PREROUTING ACCEPT [0:0]
:solana-nodes - [0:0]
-A PREROUTING -p udp -m udp --dport 8004 -j solana-nodes
-A PREROUTING -p udp -m udp --dport 8005 -j solana-nodes
-A PREROUTING -p udp -m udp --dport 8006 -j solana-nodes
-A solana-nodes -m set --match-set solana-high-staked src -j MARK --set-xmark 0x9/0xffffffff
-A solana-nodes -m set --match-set solana-staked src -j MARK --set-xmark 0x3/0xffffffff
-A solana-nodes -m set --match-set solana-unstaked src -j MARK --set-xmark 0x1/0xffffffff
COMMIT
```

## Example systemd service file 

The example file creates a service that runs the tpu-traffic-classifier.service

```
[Unit]
Description=TPU traffic classifier
After=network-online.target
StartLimitInterval=0
StartLimitIntervalSec=0

[Service]
Type=simple
User=root
Group=root
PermissionsStartOnly=true
ExecStart=/usr/local/sbin/tpu-traffic-classifier -config-file /etc/tpu-traffic-classifier/config.yml -pubkey <pubkey>

SyslogIdentifier=tpu-traffic-classifier
KillMode=process
Restart=always
RestartSec=5

LimitNOFILE=700000
LimitNPROC=700000

ProtectSystem=full

[Install]
WantedBy=multi-user.target
```

## Example iptables monitoring

You can create a script to watch the traffic go through all the various "stake classes" rules that you created and gives a nice overview of what kind of traffic is hitting your node

```
#!/bin/bash

watch -n 1 iptables -n -v -L "${1}"
```

## Recommended RPC node config

RPC nodes shouldn't expose TPU and TPUfwd (as they don't process TPU traffic into blocks) and should only receive traffic via sendTransaction.

You can use this tool to enforce this kind of firewall:

```
./tpu-traffic-classifier -config-file config.yml -our-localhost -tpu-policy DROP -fwd-policy DROP -tpu-quic-policy DROP -update=false
```

This mode will not keep the ipsets updated and will only create firewall rules for your RPC node to not accept traffic via TPU and TPUfwd.


This software repository is provided “as is". Use the software at your own risk.

