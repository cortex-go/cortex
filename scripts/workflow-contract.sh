#!/bin/sh
# Structurally validates the read-only CI workflow (.github/workflows/hardening.yml).
#
# Comment-aware parser that confirms the ACTIVE configuration (a naive grep
# could be satisfied by a commented-out line):
#   - ignores YAML comments and shell comments inside multiline run blocks;
#   - requires an active push-to-main trigger and an active pull_request trigger;
#   - requires the effective top-level permission to be read-only and rejects
#     any write permission;
#   - requires every `uses:` reference to end in a full 40-hex commit SHA;
#   - requires every mandatory gate to be an active `run:` step, either a
#     single-line run value or a command inside a multiline `run: |` body;
#   - requires the workflow-contract script itself to be an active step.
# Runs as a CI step so a regression fails the pipeline. Direct and unmasked.
set -eu

FILE=${1:-.github/workflows/hardening.yml}
[ -f "$FILE" ] || { echo "missing $FILE" >&2; exit 1; }

awk '
function trim(s){ sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
BEGIN {
  required_inline["go vet ./..."]=1
  required_inline["go test ./..."]=1
  required_inline["go test -race ./..."]=1
  required_inline["go build -trimpath -o /tmp/cortex ./cmd/cortex"]=1
  required_inline["node --check content/assets/js/script.js"]=1
  required_inline["node --check public/assets/js/script.js"]=1
  required_inline["node tests/agent-ux.test.js"]=1
  required_inline["node tests/conversation-save.test.js"]=1
  required_inline["node tests/markdown-render-smoke.js"]=1
  required_inline["tests/release-smoke.sh"]=1
  required_inline["git diff --check"]=1
  required_inline["scripts/workflow-contract.sh"]=1
  required_inline["diff -q content/assets/js/script.js public/assets/js/script.js"]=1
  required_inline["diff -q content/assets/css/style.css public/assets/css/style.css"]=1
  required_block["gofmt -l"]=1
  in_on=0; in_permissions=0; in_jobs=0
  in_push=0; in_branches=0; in_block=0
  in_setupgo=0; job_perms_seen=0
  push_main=0; seen_pull_request=0; top_contents_read=0; top_perm_seen=0; top_perm_bad=0
  uses_count=0; go_version_file=0; bad=0
}
{
  line=$0
  ws=0
  while (substr(line,ws+1,1)==" " || substr(line,ws+1,1)=="\t") ws++
  content=substr(line,ws+1)
  # Drop a trailing comment introduced by whitespace then hash. The single-line
  # commands and uses refs matched here contain no quoted hash sequences.
  i=index(content, " #")
  if (i>0) content=substr(content,1,i-1)
  content=trim(content)
  if (content=="" || substr(content,1,1)=="#") content=""

  if (in_block) {
    if (content=="") next
    if (ws <= block_indent) { in_block=0 }
    else {
      for (cmd in required_block) if (index(content, cmd)>0) found_block[cmd]=1
      next
    }
  }
  if (content=="") next

  # Top-level keys (column 0) start a section.
  if (ws==0 && content ~ /^[A-Za-z0-9_-]+:/) {
    key=content; sub(/:.*/, "", key)
    in_on=(key=="on")?1:0
    in_permissions=(key=="permissions")?1:0
    in_jobs=(key=="jobs")?1:0
    in_push=0; in_branches=0
    next
  }

  if (in_on) {
    if (content ~ /^push:/) { in_push=1; push_indent=ws; in_branches=0; if (index(content,"main")>0) push_main=1; next }
    if (content ~ /^pull_request:[ \t]*$/) { seen_pull_request=1; in_push=0; next }
    if (in_push) {
      if (ws <= push_indent) { in_push=0 }
      else {
        if (content ~ /^branches:/) { in_branches=1; if (index(content,"main")>0) push_main=1; next }
        if (in_branches && content ~ /^[-\[]/ && index(content,"main")>0) push_main=1
        next
      }
    }
    next
  }

  if (in_permissions) {
    # Exact active top-level permission contract: contents: read. Track whether
    # any top-level permission key is present and reject anything but the exact
    # "contents: read" (e.g. "issues: read", "contents: write", "actions: read").
    if (content ~ /^[A-Za-z0-9_-]+:[ \t]*[^ \t]+/) {
      top_perm_seen=1
      if (content=="contents: read") { top_contents_read=1 }
      else { top_perm_bad=1; print "invalid top-level permission: " content > "/dev/stderr" }
    }
    next
  }

  if (in_jobs) {
    if (content ~ /^permissions:/) { job_perms_seen=1; print "job-level permissions: block present (escalation risk)" > "/dev/stderr"; next }
    if (content ~ /^-[ \t]*uses:[ \t]*[^ \t]+/) {
      ref=content; sub(/^-[ \t]*uses:[ \t]*/, "", ref); sub(/^.*@/, "", ref)
      uses_count++
      if (ref !~ /^[0-9a-fA-F]{40}$/) { bad=1; print "uses: reference is not a full 40-hex SHA: " content > "/dev/stderr" }
      # The pinned setup-go step must be followed by go-version-file: go.mod.
      if (index(ref, "0a12ed9d6a96ab950c8f026ed9f722fe0da7ef32")>0) in_setupgo=1
      else in_setupgo=0
      next
    }
    if (in_setupgo && content ~ /^go-version-file:[ \t]+go\.mod[ \t]*$/) { go_version_file=1; in_setupgo=0; next }
    if (content ~ /^run:[ \t]*\|[ \t]*$/) { in_block=1; block_indent=ws; next }
    if (content ~ /^run:[ \t]+/) {
      cmd=content; sub(/^run:[ \t]+/, "", cmd); cmd=trim(cmd)
      if (cmd in required_inline) found_inline[cmd]=1
    }
    next
  }
}
END {
  fail=0
  if (!push_main) { print "missing active push trigger targeting main" > "/dev/stderr"; fail=1 }
  if (!seen_pull_request) { print "missing active pull_request trigger" > "/dev/stderr"; fail=1 }
  if (!top_perm_seen || !top_contents_read) { print "workflow top-level permission is not exactly contents: read" > "/dev/stderr"; fail=1 }
  if (top_perm_bad) fail=1
  if (job_perms_seen) fail=1
  if (uses_count==0) { print "no uses: references found" > "/dev/stderr"; fail=1 }
  if (!go_version_file) { print "missing active go-version-file: go.mod beneath pinned setup-go" > "/dev/stderr"; fail=1 }
  for (cmd in required_inline) if (!(cmd in found_inline)) { print "missing active run step: " cmd > "/dev/stderr"; fail=1 }
  for (cmd in required_block) if (!(cmd in found_block)) { print "missing active run block command: " cmd > "/dev/stderr"; fail=1 }
  exit (fail==0 && bad==0 ? 0 : 1)
}
' "$FILE"
echo "cortex hardening workflow contract: ok"