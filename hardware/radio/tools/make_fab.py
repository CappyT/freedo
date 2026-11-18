#!/usr/bin/env python3
"""Write the JLCPCB fabrication and assembly package to out/fab/<name>/.

Usage: make_fab.py [--variant NAME] [--hand REF,REF]   (KICAD_CLI overrides the kicad-cli command line)

--variant NAME  build a KiCad design variant: its DNP parts leave the BOM, the CPL and the stencil.
                Without it the package holds every part. The project defines the variant "usb".
--hand REFS     move these JLC parts to the hand-solder list (U1,U201 makes the order fit Economic PCBA).
<name> is the variant name or "full", plus "-hand" with --hand. Every --hand set of one variant writes to
the same directory: a second set replaces the first.

Outputs: freedo-radio-gerbers.zip, jlc-bom.csv, jlc-cpl.csv, hand-solder.csv.
The BOM and CPL hold only Asm=JLC parts that are not DNP. Rotation and
position corrections come from jlc_rotations.csv next to this script.
Its rows are checked against the EasyEDA land of each LCSC number:
curl -A curl/8 'https://easyeda.com/api/products/<LCSC>/components?version=6.4.19.5'
gives result.packageDetail.dataStr, PAD shapes in 10 mil units around head.x/head.y
(a browser user agent gets HTTP 403). JLC rotation is counter-clockwise positive.
"""
import argparse
import csv
import math
import os
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import xml.etree.ElementTree as ET
import zipfile
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BOARD = ROOT / "freedo-radio.kicad_pcb"
SCH = ROOT / "freedo-radio.kicad_sch"
ROTATIONS = Path(__file__).with_name("jlc_rotations.csv")
BOARD_COUNTS = (2, 5)
LAYERS = "F.Cu,In1.Cu,In2.Cu,B.Cu,F.Paste,B.Paste,F.Silkscreen,B.Silkscreen,F.Mask,B.Mask,Edge.Cuts"


def kicad_cli(*args):
    if os.environ.get("KICAD_CLI"):
        cmd = shlex.split(os.environ["KICAD_CLI"])
    elif shutil.which("kicad-cli"):
        cmd = ["kicad-cli"]
    else:
        cmd = ["flatpak", "run", f"--filesystem={ROOT}", "--command=kicad-cli", "org.kicad.KiCad"]
    subprocess.run(cmd + [str(a) for a in args], check=True, stdout=subprocess.DEVNULL)


def natural(ref):
    head = ref.rstrip("0123456789")
    return head, int(ref[len(head):] or 0)


def variant_board(variant):
    """Return the board text with the variant DNP flag as the base flag of each footprint, and their references.

    kicad-cli plots the paste layer by the base flag (rule no_paste_on_dnp), not by the variant.
    """
    marker = f'(variant\n\t\t\t(name "{variant}")\n\t\t\t(dnp yes)'
    chunks = BOARD.read_text(encoding="utf-8").split("\n\t(footprint ")
    refs = set()
    for i, chunk in enumerate(chunks):
        if i and marker in chunk:
            chunk, n = re.subn(r"\n\t\t\(attr ([^)]*)\)", r"\n\t\t(attr \1 dnp)", chunk, count=1)
            if n != 1:
                sys.exit(f"variant {variant}: a footprint has no attr line")
            refs.add(re.search(r'\(property "Reference" "([^"]+)"', chunk).group(1))
            chunks[i] = chunk
    return "\n\t(footprint ".join(chunks), refs


def export(out, netlist, board_text):
    work, gerbers = out / "kicad", out / "gerbers"
    shutil.rmtree(out, ignore_errors=True)
    for d in (work, gerbers):
        d.mkdir(parents=True)
    shutil.copy(netlist, work / "netlist.xml")
    kicad_cli("pcb", "export", "pos", "--format", "csv", "--units", "mm", "--side", "both",
              "-o", work / "pos.csv", BOARD)
    board = BOARD
    if board_text:
        # The project and rule files go with the copy, because KiCad finds them by the board name.
        board = work / BOARD.name
        board.write_text(board_text, encoding="utf-8")
        for ext in (".kicad_pro", ".kicad_dru"):
            shutil.copy(BOARD.with_suffix(ext), work)
    # Settings from JLC "How to Generate Gerber and Drill Files From KiCad 9" (2026-09-24): Protel names,
    # X2 and netlist attributes on, no job file, 4.6 mm; Excellon mm decimal, absolute, separate PTH/NPTH.
    kicad_cli("pcb", "export", "gerbers", "--layers", LAYERS, "--check-zones", "--precision", "6",
              "-o", gerbers, board)
    for job in gerbers.glob("*.gbrjob"):
        job.unlink()
    kicad_cli("pcb", "export", "drill", "--format", "excellon", "--drill-origin", "absolute",
              "--excellon-units", "mm", "--excellon-zeros-format", "decimal",
              "--excellon-oval-format", "alternate", "--excellon-separate-th", "-o", gerbers, board)
    with zipfile.ZipFile(out / "freedo-radio-gerbers.zip", "w", zipfile.ZIP_DEFLATED) as z:
        for f in sorted(gerbers.iterdir()):
            z.write(f, f.name)


def parts(netlist, variant, hand):
    out = []
    for comp in ET.parse(netlist).getroot().iter("comp"):
        props = {p.get("name"): p.get("value", "") for p in comp.findall("property")}
        out.append({
            "ref": comp.get("ref"),
            "value": comp.findtext("value", ""),
            "footprint": comp.findtext("footprint", "").split(":")[-1],
            "mpn": props.get("MPN", ""),
            "lcsc": props.get("LCSC", ""),
            "asm": "hand" if comp.get("ref") in hand else props.get("Asm", ""),
            "dnp": "dnp" in props,
            "variant_dnp": any(v.get("name") == variant and p.get("name") == "dnp" and p.get("value") == "1"
                               for v in comp.iter("variant") for p in v.findall("property")),
            "in_bom": "exclude_from_bom" not in props,
        })
    return out


def write_csv(path, header, rows):
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(header)
        w.writerows(rows)


def bom(out, jlc):
    groups = defaultdict(list)
    for p in jlc:
        groups[p["lcsc"]].append(p)
    rows = []
    for lcsc, ps in groups.items():
        if len({(p["value"], p["footprint"]) for p in ps}) != 1:
            sys.exit(f"LCSC {lcsc} has several values or footprints: {[p['ref'] for p in ps]}")
        refs = sorted((p["ref"] for p in ps), key=natural)
        rows.append([ps[0]["value"], ",".join(refs), ps[0]["footprint"], lcsc])
    rows.sort(key=lambda r: natural(r[1].split(",")[0]))
    write_csv(out / "jlc-bom.csv", ["Comment", "Designator", "Footprint", "LCSC Part #"], rows)
    return len(rows)


def read_csv(path):
    with open(path, newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def cpl(out, jlc):
    # One footprint can map to several EasyEDA lands (and rotations), so the key is the LCSC number too.
    corr = {(r["footprint"], lcsc): r for r in read_csv(ROTATIONS) for lcsc in r["lcsc"].split()}
    pos = {r["Ref"]: r for r in read_csv(out / "kicad" / "pos.csv")}
    errors = [f"{p['ref']}: no checked JLC rotation for {p['footprint']} with {p['lcsc']} in {ROTATIONS.name}"
              for p in jlc if (p["footprint"], p["lcsc"]) not in corr]
    errors += [f"{p['ref']}: not in the board position file" for p in jlc if p["ref"] not in pos]
    errors += [f"{p['ref']}: on {pos[p['ref']]['Side']}; the corrections are checked for the top side only"
               for p in jlc if p["ref"] in pos and pos[p["ref"]]["Side"] != "top"]
    if errors:
        sys.exit("\n".join(errors))
    rows = []
    for p in sorted(jlc, key=lambda p: natural(p["ref"])):
        c = corr[(p["footprint"], p["lcsc"])]
        r = pos[p["ref"]]
        rot = float(r["Rot"])
        a = math.radians(rot)
        dx, dy = float(c["dx"]), float(c["dy"])
        # dx/dy are footprint-local (KiCad Y down); the position file has Y up.
        x = float(r["PosX"]) + dx * math.cos(a) + dy * math.sin(a)
        y = float(r["PosY"]) + dx * math.sin(a) - dy * math.cos(a)
        rows.append([p["ref"], f"{x:.4f}mm", f"{y:.4f}mm", "Top", f"{(rot + float(c['rotation'])) % 360:g}"])
    write_csv(out / "jlc-cpl.csv", ["Designator", "Mid X", "Mid Y", "Layer", "Rotation"], rows)
    return len(rows)


def check_designators(out, jlc):
    want = {p["ref"] for p in jlc}
    written = {
        "jlc-bom.csv": [d for r in read_csv(out / "jlc-bom.csv") for d in r["Designator"].split(",")],
        "jlc-cpl.csv": [r["Designator"] for r in read_csv(out / "jlc-cpl.csv")],
    }
    for name, refs in written.items():
        if len(refs) != len(set(refs)) or set(refs) != want:
            sys.exit(f"{name} designators differ from the schematic Asm=JLC set: "
                     f"missing {sorted(want - set(refs))}, extra {sorted(set(refs) - want)}, "
                     f"{len(refs) - len(set(refs))} duplicates")


def hand_list(out, all_parts, variant):
    def note(p):
        if p["dnp"]:
            return "DNP, optional"
        return f"not fitted in the {variant} variant" if p["variant_dnp"] else "fitted by hand"

    groups = defaultdict(list)
    for p in all_parts:
        if p["asm"] == "hand" or p["dnp"] or p["variant_dnp"]:
            groups[p["lcsc"] or p["ref"], note(p)].append(p)
    rows = []
    for (_, text), ps in groups.items():
        n = len(ps)
        refs = ",".join(sorted((p["ref"] for p in ps), key=natural))
        rows.append([refs, ps[0]["value"], ps[0]["mpn"] or "-", ps[0]["lcsc"] or "-", ps[0]["footprint"], text, n]
                    + [0 if ps[0]["variant_dnp"] else n * b for b in BOARD_COUNTS])
    order = ["fitted by hand", "DNP, optional"]
    rows.sort(key=lambda r: (order.index(r[5]) if r[5] in order else len(order), natural(r[0].split(",")[0])))
    write_csv(out / "hand-solder.csv", ["Designator", "Value", "MPN", "LCSC Part #", "Footprint", "Note",
                                        "Per board"] + [f"Qty {b} boards" for b in BOARD_COUNTS], rows)
    return len(rows)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--variant", default="", help="KiCad design variant, for example usb")
    ap.add_argument("--hand", default="", help="comma-separated references that move to the hand-solder list")
    args = ap.parse_args()
    hand = {r for r in args.hand.split(",") if r}
    out = ROOT / "out" / "fab" / ((args.variant or "full") + ("-hand" if hand else ""))

    # The arguments, the netlist and the board are checked before the old package is deleted.
    # The temporary directory lies in the project: the flatpak sees only that tree.
    out.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(dir=out.parent) as tmp:
        netlist = Path(tmp) / "netlist.xml"
        kicad_cli("sch", "export", "netlist", "--format", "kicadxml", "-o", netlist, SCH)
        all_parts = parts(netlist, args.variant, hand)
        variant_dnp = {p["ref"] for p in all_parts if p["variant_dnp"]}
        if args.variant and not variant_dnp:
            sys.exit(f"variant {args.variant}: no schematic symbol is DNP in it")
        unknown = hand - {p["ref"] for p in all_parts}
        if unknown:
            sys.exit(f"--hand: not in the schematic: {sorted(unknown)}")
        board_text, board_dnp = variant_board(args.variant) if args.variant else ("", set())
        if variant_dnp != board_dnp:
            sys.exit(f"variant {args.variant}: schematic and board differ on {sorted(variant_dnp ^ board_dnp)}")
        export(out, netlist, board_text)
    jlc = [p for p in all_parts if p["asm"] == "JLC" and not p["dnp"] and not p["variant_dnp"]]
    bad = [p["ref"] for p in jlc if not p["lcsc"] or not p["in_bom"]]
    if bad:
        sys.exit(f"Asm=JLC parts without LCSC or excluded from the BOM: {bad}")
    lines = bom(out, jlc)
    placed = cpl(out, jlc)
    check_designators(out, jlc)
    rows = hand_list(out, all_parts, args.variant)
    print(f"OK: {len(jlc)} JLC parts on {lines} BOM lines, {placed} CPL rows, {rows} hand-solder lines, "
          f"{len(list((out / 'gerbers').iterdir()))} files in freedo-radio-gerbers.zip, output in {out}")


if __name__ == "__main__":
    main()
