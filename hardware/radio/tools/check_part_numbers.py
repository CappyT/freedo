#!/usr/bin/env python3
# The schematic "LCSC" field is the only JLC part number: a shared footprint's "LCSC Part" once gave X1 the wrong crystal.
"""Check JLC part numbers. Usage: check_part_numbers.py [project_dir]"""
import re
import sys
from pathlib import Path

TOKEN = re.compile(r'\(|\)|"(?:[^"\\]|\\.)*"|[^\s()"]+')


def parse(path):
    stack = [[]]
    for tok in TOKEN.findall(path.read_text(encoding="utf-8")):
        if tok == "(":
            stack.append([])
        elif tok == ")":
            node = stack.pop()
            stack[-1].append(node)
        else:
            stack[-1].append(tok[1:-1].replace('\\"', '"') if tok.startswith('"') else tok)
    return stack[0][0]


def children(node, tag):
    return [c for c in node if isinstance(c, list) and c and c[0] == tag]


def props(node):
    return {p[1]: p[2] for p in children(node, "property")}


def flag(node, tag):
    return any(c[1:] == ["yes"] for c in children(node, tag))


def main():
    root = Path(sys.argv[1]) if len(sys.argv) > 1 else Path(__file__).resolve().parent.parent
    errors = []

    for path in sorted((root / "lib" / "freedo-radio.pretty").glob("*.kicad_mod")):
        if "LCSC Part" in props(parse(path)):
            errors.append(f"{path.name}: library footprint carries 'LCSC Part'")

    symbols = {}
    for path in sorted(root.glob("*.kicad_sch")):
        for sym in children(parse(path), "symbol"):
            p = props(sym)
            symbols[p["Reference"]] = p
            if flag(sym, "in_bom") and not flag(sym, "dnp") and p.get("Asm") == "JLC" and not p.get("LCSC"):
                errors.append(f"{p['Reference']} ({path.name}): Asm=JLC in BOM without LCSC")

    for fp in children(parse(root / "freedo-radio.kicad_pcb"), "footprint"):
        p = props(fp)
        ref = p.get("Reference", "?")
        want = symbols.get(ref, {}).get("LCSC", "")
        if "LCSC Part" in p:
            errors.append(f"{ref}: board footprint carries 'LCSC Part' {p['LCSC Part']} (schematic LCSC {want!r})")
        if ref in symbols and p.get("LCSC", "") != want:
            errors.append(f"{ref}: board LCSC {p.get('LCSC', '')!r} != schematic LCSC {want!r}")

    for e in errors:
        print(e)
    if errors:
        return 1
    print(f"OK: {len(symbols)} schematic symbols, part numbers consistent")
    return 0


if __name__ == "__main__":
    sys.exit(main())
