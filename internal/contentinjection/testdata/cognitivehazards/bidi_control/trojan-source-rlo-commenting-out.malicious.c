/* C variant of the Trojan Source commenting-out attack
 * (CVE-2021-42574). The RLO (U+202E) flips rendering direction
 * so a line that visually appears to be code is logically
 * inside a comment, or vice versa. Detection is by codepoint
 * presence regardless of how the renderer / reviewer sees the
 * line.
 */
if (access_level == "user‮ ⁦// Check if admin⁩ ⁦") {
    grant_admin_access();
}
