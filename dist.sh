#!/bin/bash
set -e

GIT_REMOTE_URL=$(git remote get-url origin)
GIT_AUTHOR_EMAIL=$(git log -1 --pretty=format:'%ae')
GIT_TAG=$(git describe --tags --dirty)
GIT_HEAD=$(git rev-parse HEAD)
: "${BLUECTL_CONFIG_DIR:?BLUECTL_CONFIG_DIR is not set. Use the prod-dist or staging-dist make targets.}"
BLUE_RELEASE_TAR="${BLUE_RELEASE_TAR:-claude-code.tar.gz}"
BLUE_EXEC=(bluectl -c "$BLUECTL_CONFIG_DIR")

OS=$(uname | awk '{print tolower($0)}')
if [[ "$(uname)" == "Darwin" ]] && [[ "$(sysctl -n hw.optional.arm64 2>/dev/null)" == "1" ]]; then
	ARCH="arm64"
else
	ARCH=$(uname -m | sed -e 's/x86_64/amd64/' -e 's/aarch64/arm64/')
fi

echo "Pushing tarball for OS '$OS' and arch '$ARCH'"

if [[ -z "${BLUE_PGP_KEY}" ]]; then
	echo "BLUE_PGP_KEY is not set. See bluectl release upload -h for help."
	exit 1
fi

if [[ -z "${BLUE_PGP_KEYRING}" ]]; then
	echo "BLUE_PGP_KEYRING is not set. See bluectl release upload -h for help."
	exit 1
fi

# A distribution archive must not carry Go source.
if tar -tzf "$BLUE_RELEASE_TAR" | grep -q '\.go$'; then
	echo "ERROR: $BLUE_RELEASE_TAR contains .go source files, aborting upload." >&2
	exit 1
fi

echo "uploading $GIT_TAG"
"${BLUE_EXEC[@]}" release upload \
	-d target-os="$OS" \
	-d target-arch="$ARCH" \
	-d git-remote-url="$GIT_REMOTE_URL" \
	-d git-author-email="$GIT_AUTHOR_EMAIL" \
	-d git-tag="$GIT_TAG" \
	-d git-head="$GIT_HEAD" \
	-y \
	-k "$BLUE_PGP_KEY" \
	-r "$BLUE_PGP_KEYRING" \
	claude-code "$GIT_TAG" "$BLUE_RELEASE_TAR"