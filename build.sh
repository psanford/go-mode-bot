#!/bin/bash

set -e
set -x
set -o pipefail

# PR should be set in the environment

repo=https://github.com/dominikh/go-mode.el
srcdir=go-mode.el

tmpdir=$(mktemp -d)

cd $tmpdir

gotar=go1.12.8.src.tar.gz
wget https://dl.google.com/go/$gotar
tar xf $gotar

git clone https://github.com/psanford/emacs-batch-reindent

git clone $repo $srcdir

(
  cd $srcdir

  branch=pr/$PR

  git fetch -fu origin refs/pull/$PR/head:$branch
  git checkout $branch

  git log master..HEAD > /artifacts/git_log
)

(
  cd emacs-batch-reindent

  GO_MODE="../go-mode.el/go-mode.el"
  EXT=".go"
  FORMAT_DIR="../go/src/cmd"

  set +e

  start="$(date +%s)"
  time printf "$FORMAT_DIR\n$EXT\n" | emacs --batch -q -l $GO_MODE -l batch-reindent.el -f batch-reindent >/artifacts/batch-reindent.log 2>&1
  result=$?
  set -e
  end="$(date +%s)"
  delta=$((end-start))
  echo $delta > /artifacts/batch-reindent.runtime
  echo $result > /artifacts/batch-reindent.exitcode
)

(
  cd go

  git diff > /artifacts/batch-reindent.diff
)

(
  cd go-mode.el/test

  set +e
  start="$(date +%s)"
  emacs --batch -q -l ert -l ../go-mode.el -l go-indentation-test.el -f ert-run-tests-batch-and-exit >/artifacts/emacs-tests.log 2>&1
  result=$?
  set -e
  end="$(date +%s)"
  delta=$((end-start))
  echo $delta >> /artifacts/emacs-tests.runtime
  echo $result > /artifacts/emacs-tests.exitcode
)
