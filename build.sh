#!/bin/bash

set -e
set -x
set -o pipefail

# PR should be set in the environment as a plain number: 1338
# REPO should be set in the environment as: owner/project

fullrepo=https://github.com/${REPO-dominikh/go-mode.el}
srcdir=go-mode.el

tmpdir=$(mktemp -d)

cd $tmpdir

gotar=go1.12.8.src.tar.gz
wget https://dl.google.com/go/$gotar
mkdir go_orig go
tar xf $gotar -C go_orig --strip-components=1
tar xf $gotar -C go --strip-components=1

git clone https://github.com/psanford/emacs-batch-reindent

git clone $fullrepo $srcdir

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
  FORMAT_DIR="../go/src"

  set +e
  start="$(date +%s+N)"
  echo "dir: $FORMAT_DIR" >> /artifacts/batch-reindent.log
  echo "ext: $EXT" >> /artifacts/batch-reindent.log
  time printf "$FORMAT_DIR\n$EXT\n" | emacs --batch -q -l $GO_MODE -l batch-reindent.el -f batch-reindent >>/artifacts/batch-reindent.log 2>&1
  result=$?
  set -e
  end="$(date +%s+N)"
  delta=$((end-start)/1000000)
  echo $delta > /artifacts/batch-reindent.runtime
  echo $result > /artifacts/batch-reindent.exitcode
)

(
  set +e
  diff -u -r go_orig go > /artifacts/batch-reindent.diff
  result=$?
  set -e
  # 0 == no diff; 1 == diff; 2 == problem
  if [ $result -ge 2 ]; then
    exit $result
  fi

  diffstat /artifacts/batch-reindent.diff > /artifacts/batch-reindent.diffstat
)

(
  cd go-mode.el/test

  set +e
  start="$(date +%s+N)"
  emacs --batch -q -l ert -l ../go-mode.el -l go-indentation-test.el -f ert-run-tests-batch-and-exit >/artifacts/emacs-tests.log 2>&1
  result=$?
  set -e
  end="$(date +%s+N)"
  delta=$((end-start)/1000000)
  echo $delta >> /artifacts/emacs-tests.runtime
  echo $result > /artifacts/emacs-tests.exitcode
)
