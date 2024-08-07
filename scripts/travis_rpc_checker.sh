#!/usr/bin/env bash
set -e

TEST_REPO_BRANCH=${TEST_REPO_BRANCH:-master}
echo "Working on '${TEST_REPO_BRANCH}' branch"
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
echo "Working dir is ${DIR}"
echo "GOPATH is ${GOPATH}"
cd "${GOPATH}/src/github.com/harmony-one/harmony-test"
# cover possible force pushes to remote branches - just rebase local on top of origin
git checkout "${TEST_REPO_BRANCH}"
git pull --rebase=true
cd localnet
docker build --build-arg MAIN_REPO_BRANCH="$(git rev-parse --abbrev-ref HEAD)" -t harmonyone/localnet-test .
docker run -v "$DIR/../:/go/src/github.com/harmony-one/harmony" harmonyone/localnet-test -n
