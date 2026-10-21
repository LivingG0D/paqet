#!/usr/bin/env bash
# paqet is now paqet-plus. This file keeps its old name so installs and links that still fetch
# setup-paqet.sh (the old paqet-ctl's Update, older READMEs) keep working: it runs
# setup-paqet-plus.sh, which moves an existing paqet install to the new name.
set -euo pipefail
BRANCH="${PAQET_PLUS_BRANCH:-${PAQET_BRANCH:-master}}"
exec bash <(curl -fsSL "https://raw.githubusercontent.com/LivingG0D/paqet-plus/$BRANCH/setup-paqet-plus.sh")
