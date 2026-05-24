# Python adaptation of the canonical Trojan Source PoC
# (CVE-2021-42574). The Wikipedia article's example uses RLI
# (U+2067) inside a docstring to make the function appear to
# document its return semantics, while the actual `return`
# statement lands inside what visually appears to be the docstring.
def sum(num1, num2):
    '''Add num1 and num2, and ⁧ ''' ;return
    return num1 + num2
