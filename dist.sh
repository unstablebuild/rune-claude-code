#!/bin/bash
set -e

GIT_REMOTE_URL=$(git remote get-url origin)
GIT_AUTHOR_EMAIL=$(git log -1 --pretty=format:'%ae')
GIT_TAG=$(git describe --tags --dirty)
GIT_HEAD=$(git rev-parse HEAD)
: "${BLUECTL_CONFIG_DIR:?BLUECTL_CONFIG_DIR is not set. Use the dist-<env>-<os>-<arch> make targets (e.g. dist-prod-darwin-arm64) so the bluectl project-id is pinned to the right environment.}"
: "${BLUE_TARGET_OS:?BLUE_TARGET_OS is not set. Use the dist-<env>-<os>-<arch> make targets.}"
: "${BLUE_TARGET_ARCH:?BLUE_TARGET_ARCH is not set. Use the dist-<env>-<os>-<arch> make targets.}"
BLUE_RELEASE_TAR="${BLUE_RELEASE_TAR:-claude-code.tar.gz}"
BLUE_EXEC=(bluectl -c "$BLUECTL_CONFIG_DIR")
OS="$BLUE_TARGET_OS"
ARCH="$BLUE_TARGET_ARCH"

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

# The log since the previous tag, or since the latest tag when HEAD is not
# tagged.
if git describe --tags --exact-match HEAD >/dev/null 2>&1; then
	GIT_LOG_RANGE="$(git tag -l --sort=-version:refname | head -2 | xargs | sed 's/ /.../g')"
else
	GIT_LOG_RANGE="$(git tag -l --sort=-version:refname | head -1)...HEAD"
fi
# bluectl's -d rejects values that contain "=".
GIT_LOG=$(git log --pretty=format:"%h: %s" $GIT_LOG_RANGE | tr '=' ':')
printf 'using git range %s:\n%s\n' "$GIT_LOG_RANGE" "$GIT_LOG"

echo "uploading $GIT_TAG"
# bluectl prompts for the PGP passphrase next. Under Rune, mark the tab as
# waiting on the user so the prompt reaches them as a notification.
if command -v runectl >/dev/null; then
	runectl status blocked --kind auth --app bluectl \
		"Enter the PGP passphrase to upload claude-code $GIT_TAG for $OS/$ARCH" || true
fi
"${BLUE_EXEC[@]}" release upload \
	-d target-os="$OS" \
	-d target-arch="$ARCH" \
	-d git-remote-url="$GIT_REMOTE_URL" \
	-d git-author-email="$GIT_AUTHOR_EMAIL" \
	-d git-tag="$GIT_TAG" \
	-d git-head="$GIT_HEAD" \
	-d git-log="$GIT_LOG" \
	-y \
	-k "$BLUE_PGP_KEY" \
	-r "$BLUE_PGP_KEYRING" \
	claude-code "$GIT_TAG" "$BLUE_RELEASE_TAR"