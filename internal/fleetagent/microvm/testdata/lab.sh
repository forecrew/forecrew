#!/bin/bash
# A host with two machines, a LAN with a metadata endpoint and the internet, in
# namespaces: run as
#   unshare -Urnm -pf --mount-proc bash lab.sh <rules of slot 0> <rules of slot 1>
# Veth pairs stand in for the taps; machine 1 stands for another organization's.
# Prints REACH or blocked for each target, as machine 0 (172.30.0.2) sees it,
# and then how many connections the mining counter of machine 0 saw.
set -u
ip link set lo up
sysctl -qw net.ipv4.ip_forward=1
sysctl -qw net.ipv6.conf.all.forwarding=1
mkns() { unshare -n sleep 300 >/dev/null 2>&1 & echo $!; }
nsA=$(mkns); nsB=$(mkns); nsL=$(mkns); nsP=$(mkns); sleep 0.5
inns() { local p=$1; shift; nsenter -t "$p" -n "$@"; }
for p in $nsA $nsB $nsL $nsP; do inns "$p" ip link set lo up; done
link() { # host-side name, namespace, host address, peer address, peer gateway
	ip link add "$1" type veth peer name peer0
	ip link set peer0 netns "$2"
	ip addr add "$3" dev "$1"; ip link set "$1" up
	inns "$2" ip addr add "$4" dev peer0; inns "$2" ip link set peer0 up
	inns "$2" ip route add default via "$5"
}
link fcvm0 "$nsA" 172.30.0.1/30 172.30.0.2/30 172.30.0.1
link fcvm1 "$nsB" 172.30.0.5/30 172.30.0.6/30 172.30.0.5
link vlan "$nsL" 192.168.50.10/24 192.168.50.20/24 192.168.50.10
link vpub "$nsP" 203.0.113.1/24 203.0.113.5/24 203.0.113.1
inns "$nsP" ip addr add 203.0.113.6/24 dev peer0
# The cloud's metadata endpoints, on the LAN side, in IPv4 and IPv6.
ip addr add 169.254.0.1/16 dev vlan
inns "$nsL" ip addr add 169.254.169.254/16 dev peer0
ip addr add fd00:ec2::1/64 dev vlan nodad
inns "$nsL" ip addr add fd00:ec2::254/64 dev peer0 nodad
inns "$nsL" ip -6 route add default via fd00:ec2::1
inns "$nsA" ip addr add fd00:a::2/64 dev peer0 nodad
inns "$nsA" ip -6 route add default via fd00:a::1
ip addr add fd00:a::1/64 dev fcvm0 nodad
listen() { # namespace (or "-" for the host), ports...
	local ns=$1; shift
	local run=(); [ "$ns" = - ] || run=(nsenter -t "$ns" -n)
	"${run[@]}" python3 -c '
import socket, select, sys
ls = []
for port in map(int, sys.argv[1:]):
    for fam in (socket.AF_INET, socket.AF_INET6):
        s = socket.socket(fam); s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        if fam == socket.AF_INET6: s.setsockopt(socket.IPPROTO_IPV6, socket.IPV6_V6ONLY, 1)
        s.bind(("::" if fam == socket.AF_INET6 else "0.0.0.0", port)); s.listen(50); ls.append(s)
while True:
    for x in select.select(ls, [], [])[0]: x.accept()[0].close()
' "$@" >/dev/null 2>&1 &
}
listen - 2222 8080; listen "$nsL" 80; listen "$nsP" 80 3333; listen "$nsB" 9000
sleep 1
nft -f "$1" && nft -f "$2" || exit 1
t() { # address, port, what
	if inns "$nsA" timeout 4 python3 -c 'import socket,sys; socket.create_connection((sys.argv[1], int(sys.argv[2])), timeout=2)' "$1" "$2" 2>/dev/null; then
		echo "REACH $3"
	else
		echo "blocked $3"
	fi
}
t 203.0.113.5 80 internet
t 192.168.50.20 80 lan
t 192.168.50.10 8080 forecrewd
t 192.168.50.10 2222 host-lan-other-port
t 203.0.113.1 2222 host-public-other-port
t 172.30.0.1 2222 own-tap
t 172.30.0.5 2222 other-tap
t 172.30.0.6 9000 other-machine
t fd00:a::1 2222 host-ipv6
t 203.0.113.6 80 internet-other
t 169.254.169.254 80 metadata
t fd00:ec2::254 80 metadata-ipv6
t 203.0.113.5 3333 mining-port
echo "MINING $(nft list counter inet forecrew_vm0 mining | grep -o 'packets [0-9]*' | cut -d' ' -f2)"
