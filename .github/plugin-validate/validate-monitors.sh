#!/usr/bin/env bash
# Validate the plugin's monitor declarations (plugins/shuck/monitors/monitors.json).
#
# `claude plugin validate --strict` does not read this file — a monitor missing a
# required field, or one referencing ${user_config.*}, passes it untouched. That
# matters more than it sounds: the monitor is the plugin's primary delivery
# channel, and a monitor Claude Code rejects (or one whose command is not there)
# fails the only way a monitor can fail — in silence, on a stream nobody sees,
# leaving a session that simply never hears about a red build.
#
# So this checks what the reference actually requires:
# https://code.claude.com/docs/en/plugins-reference#monitors
set -euo pipefail

root="${1:-plugins/shuck}"
file="${root}/monitors/monitors.json"

fail() {
  echo "::error file=${file}::$1"
  exit 1
}

[ -f "$file" ] || fail "no monitors.json at ${file}"
jq -e . "$file" >/dev/null 2>&1 || fail "monitors.json is not valid JSON"
jq -e 'type == "array" and length > 0' "$file" >/dev/null ||
  fail "monitors.json must be a non-empty JSON array"

# Required fields: every entry needs a non-empty name, command and description.
jq -e 'all(.[]; (.name? // "") != "" and (.command? // "") != "" and (.description? // "") != "")' "$file" >/dev/null ||
  fail "every monitor needs a non-empty name, command and description"

# A name is what stops a reload or a repeated skill dispatch spawning a second
# process, so a duplicate would defeat the field's whole purpose.
jq -e '(map(.name) | length) == (map(.name) | unique | length)' "$file" >/dev/null ||
  fail "monitor names must be unique within the plugin"

# Claude Code rejects a monitor whose command references ${user_config.*} rather
# than substituting it, and monitor processes never receive
# CLAUDE_PLUGIN_OPTION_* — a monitor needing a configured value must read it
# from a file it owns.
jq -e 'all(.[]; (.command | contains("${user_config.")) | not)' "$file" >/dev/null ||
  fail "a monitor command may not reference \${user_config.*}"

# "always" (the default) or "on-skill-invoke:<skill-name>" are the only
# spellings, and the skill name after the colon has to be there — "on-skill-invoke:"
# alone names nothing and would never start.
jq -e 'all(.[]; (has("when") | not) or .when == "always" or (.when | startswith("on-skill-invoke:") and (length > 16)))' "$file" >/dev/null ||
  fail 'when must be "always" or "on-skill-invoke:<skill-name>"'

# Every command that runs a script out of the plugin must actually reach one:
# ${CLAUDE_PLUGIN_ROOT} resolves to the plugin root at run time, so resolve it
# here and check the file is present and executable. A command that is not a
# plugin script (a plain `tail -F …`) has nothing to check and is left alone.
while IFS= read -r cmd; do
  case "$cmd" in
  *"\${CLAUDE_PLUGIN_ROOT}"*) ;;
  *) continue ;;
  esac
  # Strip the quoting the command needs for a plugin root containing a space,
  # then take the first word: that is the script the monitor execs.
  script="${cmd//\"/}"
  script="${script%% *}"
  script="${script/\$\{CLAUDE_PLUGIN_ROOT\}/$root}"
  [ -f "$script" ] || fail "monitor command points at a missing file: ${script}"
  [ -x "$script" ] || fail "monitor command is not executable: ${script}"
  echo "  ok: ${script}"
done < <(jq -r '.[].command' "$file")

echo "✔ monitor declarations valid ($(jq -r 'length' "$file") monitor(s))"
