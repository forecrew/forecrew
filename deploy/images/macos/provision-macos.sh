#!/bin/sh
# Provisions a Forecrew macOS image over ssh as the VM's admin user: Homebrew
# toolchains, the agent CLIs at the versions package.json pins, and the worker
# with its launchd job. Files arrive in /tmp from the Packer template.
set -eu

PATH=/opt/homebrew/bin:/usr/local/bin:$PATH
export PATH
if ! command -v brew >/dev/null; then
	echo "Homebrew is not installed in the base VM" >&2
	exit 1
fi

# Homebrew installs the current formula versions; the versions in use are
# recorded in /etc/forecrew/packages and in the manifest.
brew install git go node python jq

sudo install -d -m 0755 /opt/forecrew /opt/forecrew-cli /etc/forecrew /var/lib/forecrew-worker /var/run/forecrew
sudo install -m 0644 /tmp/package.json /tmp/package-lock.json /opt/forecrew-cli/
(cd /opt/forecrew-cli && sudo env PATH="$PATH" npm ci --omit=dev --no-audit --no-fund)
labels="git go node python"
for cli in claude codex opencode; do
	sudo ln -sf "/opt/forecrew-cli/node_modules/.bin/$cli" "/usr/local/bin/$cli"
	"$cli" --version >/dev/null
	case $cli in claude) labels="$labels runtime:claude-code" ;; *) labels="$labels runtime:$cli" ;; esac
done

sudo install -m 0755 /tmp/forecrew-worker /opt/forecrew/forecrew-worker
sudo install -m 0755 /tmp/forecrew-guest/forecrew-start /opt/forecrew/forecrew-start
sudo install -m 0644 -o root -g wheel /tmp/forecrew-guest/com.forecrew.worker.plist /Library/LaunchDaemons/com.forecrew.worker.plist
for l in $labels; do echo "$l"; done | sudo tee /etc/forecrew/labels >/dev/null
/opt/forecrew/forecrew-worker version

{
	for f in git go node python jq; do
		echo "$f=$(brew list --versions "$f" | awk '{print $2}')"
	done
	for pkg in claude-code:@anthropic-ai/claude-code codex:@openai/codex opencode-ai:opencode-ai; do
		echo "${pkg%%:*}=$(node -p "require('/opt/forecrew-cli/node_modules/${pkg#*:}/package.json').version")"
	done
} | sudo tee /etc/forecrew/packages >/dev/null

# launchd loads the job at boot; it runs when the provider's settings file appears.
rm -rf /tmp/forecrew-guest /tmp/forecrew-worker /tmp/package.json /tmp/package-lock.json
