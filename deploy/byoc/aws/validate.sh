#!/bin/sh
# Lints the AWS setup kit with the tools that exist: cfn-lint on the
# CloudFormation template, tofu (or terraform) validate and fmt on the module.
# A tool that is not installed is said so and skipped; internal/fleetagent/aws
# tests the templates without any of them (make test).
set -eu

here=$(cd "$(dirname "$0")" && pwd)
ran=0

if command -v cfn-lint >/dev/null 2>&1; then
	echo "== cfn-lint"
	cfn-lint "$here/cloudformation.yaml"
	ran=1
else
	echo "cfn-lint is not installed: cloudformation.yaml not linted"
fi

tf=""
for c in tofu terraform; do
	if command -v "$c" >/dev/null 2>&1; then
		tf=$c
		break
	fi
done
if [ -n "$tf" ]; then
	echo "== $tf validate"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	cp "$here"/*.tf "$here"/*.tftpl "$tmp"/
	(cd "$tmp" && "$tf" init -backend=false -input=false >/dev/null && "$tf" validate && "$tf" fmt -check -diff)
	ran=1
else
	echo "neither tofu nor terraform is installed: the module is not validated"
fi

[ "$ran" -eq 1 ] || echo "nothing was linted"
