# The worktree rule walks up looking for .git, which a fixture cannot check in,
# so the checkout is made here. opencode's store is SQLite, built from SQL for
# the same reason the importer's own tests do: reviewable, not a binary blob.
mkdir -p .git claude oc "$HOME/plain" "$HOME/.local/share/opencode"
sed "s#{{WORKTREE}}#$PWD/oc#g" "$FIXTURE/opencode.sql" | sqlite3 "$HOME/.local/share/opencode/opencode.db"
