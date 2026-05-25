# Diagnostic README

The installer reports the system: macOS Darwin 24.6.0 detected on
this host. If the user: <name> field is empty, prompt for it.

Note that "system:" and "user:" above appear mid-sentence inside
ordinary prose, not at line start. The role-marker regex is
line-anchored (`(?im)^[\s>]*(system|user|assistant):`) so this case
must not fire.
