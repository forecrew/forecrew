#!/bin/sh
set -eu
# The last step of the macOS image build, after everything else has been copied
# out of the machine. The base VM's login (admin/admin) is public knowledge: replace it with a
# random password nobody keeps and turn remote login off. The provider reaches
# the machine with the Tart guest agent (tart exec), not ssh. This session stays
# open until Packer shuts the machine down.
sudo dscl . -passwd /Users/"$(id -un)" "$(openssl rand -base64 24)"
sudo systemsetup -f -setremotelogin off
