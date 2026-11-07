#!/bin/sh
# Provisions a Forecrew Linux image, in a build VM (FORECREW_IMAGE_KIND=vm) or a
# build container (container): git, common toolchains, the agent CLIs at the
# versions package.json pins, and the worker with its start-up files. Files
# arrive in /tmp from the Packer template.
set -eu

kind=${FORECREW_IMAGE_KIND:?FORECREW_IMAGE_KIND is vm or container}
export DEBIAN_FRONTEND=noninteractive

apt-get update
# iproute2 and kmod: the microVM init configures the network and loads modules.
apt-get install -y --no-install-recommends \
	ca-certificates curl git openssh-client unzip jq procps iproute2 kmod \
	build-essential make pkg-config python3 python3-venv python3-pip \
	golang-go xz-utils
labels="go node python git"
if [ "$kind" = vm ]; then
	apt-get install -y --no-install-recommends docker.io cloud-init
	systemctl enable docker
	labels="$labels docker"
fi

# Node: the version of the control worker image, from nodejs.org with the
# checksum pinned here (Claude Code needs Node 22 or newer, which Debian 13 does
# not have). Bump the version and both checksums together.
node_version=24.22.2
case $(uname -m) in
x86_64 | amd64) node_arch=x64 node_sha256=48cc0e21e048790e54c0cf5749c2725005313d652651c40b7a21feb9697aaf75 ;;
aarch64 | arm64) node_arch=arm64 node_sha256=eb187837a2230d4047161ac0be008584e16b46f726e9c9490ee35dc6b17d9ba8 ;;
*) echo "provision-linux.sh: no Node build pinned for $(uname -m)" >&2; exit 1 ;;
esac
node_tar=node-v$node_version-linux-$node_arch.tar.xz
curl -fsSL -o "/tmp/$node_tar" "https://nodejs.org/dist/v$node_version/$node_tar"
echo "$node_sha256  /tmp/$node_tar" | sha256sum -c -
tar -xJf "/tmp/$node_tar" -C /usr/local --strip-components=1 --no-same-owner
rm -f "/tmp/$node_tar"
[ "$(node --version)" = "v$node_version" ]

# Agent CLIs: exact versions from package.json, every dependency pinned by
# package-lock.json, owned by root so the agents cannot change them.
install -d -m 0755 /opt/forecrew-cli
install -m 0644 /tmp/package.json /tmp/package-lock.json /opt/forecrew-cli/
# Optional dependencies carry the native binaries (opencode's platform packages),
# so they are asked for by name. Install scripts run per package afterwards, so a
# failing one can be told apart and its surroundings reported.
(cd /opt/forecrew-cli && npm ci --omit=dev --include=optional --ignore-scripts --no-audit --no-fund)
for pkg in @anthropic-ai/claude-code @openai/codex opencode-ai; do
	if ! (cd /opt/forecrew-cli && npm rebuild "$pkg" --no-audit --no-fund); then
		echo "provision-linux.sh: the install script of $pkg failed; node $(node --version), npm $(npm --version), $(uname -m)" >&2
		find /opt/forecrew-cli/node_modules -maxdepth 1 -name 'opencode*' >&2
		ldd --version 2>&1 | head -1 >&2
		grep -o -m1 avx2 /proc/cpuinfo >&2 || echo "no avx2" >&2
		sed -n '/^function installPackage/,$p' "/opt/forecrew-cli/node_modules/$pkg/postinstall.mjs" >&2 || true
		find /opt/forecrew-cli/node_modules/opencode-linux-x64-baseline /opt/forecrew-cli/node_modules/opencode-ai/bin -maxdepth 2 >&2 || true
		exit 1
	fi
done
(cd /opt/forecrew-cli && npm cache clean --force)
for cli in claude codex opencode; do
	ln -sf "/opt/forecrew-cli/node_modules/.bin/$cli" "/usr/local/bin/$cli"
	"$cli" --version >/dev/null
	labels="$labels runtime:$(case $cli in claude) echo claude-code ;; *) echo "$cli" ;; esac)"
done

# The worker and the files that start it.
install -d -m 0755 /opt/forecrew /etc/forecrew /var/lib/forecrew-worker
install -m 0755 /tmp/forecrew-worker /opt/forecrew/forecrew-worker
install -m 0755 /tmp/forecrew-guest/forecrew-init /tmp/forecrew-guest/forecrew-start /opt/forecrew/
for l in $labels; do echo "$l"; done >/etc/forecrew/labels
/opt/forecrew/forecrew-worker version

# What the image holds, as name=version lines for the manifest.
{
	for p in git golang-go python3; do
		echo "$p=$(dpkg-query -W -f='${Version}' "$p")"
	done
	echo "nodejs=$node_version"
	for cli in claude-code codex opencode-ai; do
		pkg=$(case $cli in claude-code) echo @anthropic-ai/claude-code ;; codex) echo @openai/codex ;; *) echo opencode-ai ;; esac)
		echo "$cli=$(node -p "require('/opt/forecrew-cli/node_modules/$pkg/package.json').version")"
	done
} >/etc/forecrew/packages

if [ "$kind" = vm ]; then
	install -m 0644 /tmp/forecrew-guest/forecrew-worker.service /etc/systemd/system/forecrew-worker.service
	systemctl enable forecrew-worker.service
	# No identity travels with the image: each machine makes its own.
	cloud-init clean --logs --seed
	rm -f /etc/ssh/ssh_host_*
	truncate -s 0 /etc/machine-id
fi

apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/forecrew-guest /tmp/forecrew-worker /tmp/package.json /tmp/package-lock.json
