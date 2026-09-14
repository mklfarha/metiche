package cmd

// cmdUninstall runs nothing and deletes nothing: install.sh --uninstall is the
// single owner of everything the installer wrote (docs/CLI.md §1.9).
func (a *app) cmdUninstall(args []string) error {
	fs := a.flags("uninstall")
	if _, err := a.parse(fs, args); err != nil {
		return err
	}
	dry := "curl -fsSL https://metiche.xyz/install.sh | sh -s -- --uninstall --dry-run"
	real := "curl -fsSL https://metiche.xyz/install.sh | sh -s -- --uninstall"
	if a.json {
		d := doc("uninstall")
		d["commands"] = []string{dry, real}
		d["removes"] = "~/.metiche (including ~/.metiche/bin/metiche), client entries, the profile line; never .metiche files"
		a.writeJSON(d)
		return nil
	}
	a.out("metiche is removed by the installer that set it up, so nothing is left half-undone:")
	a.out("")
	a.out("  %s   # see every change", dry)
	a.out("  %s", real)
	a.out("")
	a.out("It backs up and removes ~/.metiche, including this binary (~/.metiche/bin/metiche), and contacts")
	a.out("no server. The Claude Code plugin stays installed; the uninstaller prints how to remove it.")
	a.out("")
	a.out("Neither the installer nor this command removes .metiche files: they live in your repositories.")
	return nil
}
