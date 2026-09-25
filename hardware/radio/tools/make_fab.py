#!/usr/bin/env python3
"""Write the JLCPCB fabrication and assembly package to out/fab/.

Usage: make_fab.py   (KICAD_CLI overrides the kicad-cli command line)

Outputs: freedo-radio-gerbers.zip, jlc-bom.csv, jlc-cpl.csv, hand-solder.csv.
The BOM and CPL hold only Asm=JLC parts that are not DNP. Rotation and
position corrections come from jlc_rotations.csv next to this script.
Its rows are checked against the EasyEDA land of each LCSC number:
curl -A curl/8 'https://easyeda.com/api/products/<LCSC>/components?version=6.4.19.5'
gives result.packageDetail.dataStr, PAD shapes in 10 mil units around head.x/head.y
(a browser user agent gets HTTP 403). JLC rotation is counter-clockwise positive.
"""
import csv
import math
import os
import shlex
import shutil
import subprocess
import sys
import xml.etree.ElementTree as ET
import zipfile
from collections import defaultdict
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
BOARD = ROOT / "freedo-radio.kicad_pcb"
SCH = ROOT / "freedo-radio.kicad_sch"
OUT = ROOT / "out" / "fab"
WORK = OUT / "kicad"
GERBERS = OUT / "gerbers"
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


def export():
    for d in (WORK, GERBERS):
        shutil.rmtree(d, ignore_errors=True)
        d.mkdir(parents=True)
    kicad_cli("sch", "export", "netlist", "--format", "kicadxml", "-o", WORK / "netlist.xml", SCH)
    kicad_cli("pcb", "export", "pos", "--format", "csv", "--units", "mm", "--side", "both",
              "-o", WORK / "pos.csv", BOARD)
    # Settings from JLC "How to Generate Gerber and Drill Files From KiCad 9" (2026-09-24): Protel names,
    # X2 and netlist attributes on, no job file, 4.6 mm; Excellon mm decimal, absolute, separate PTH/NPTH.
    kicad_cli("pcb", "export", "gerbers", "--layers", LAYERS, "--check-zones", "--precision", "6",
              "-o", GERBERS, BOARD)
    for job in GERBERS.glob("*.gbrjob"):
        job.unlink()
    kicad_cli("pcb", "export", "drill", "--format", "excellon", "--drill-origin", "absolute",
              "--excellon-units", "mm", "--excellon-zeros-format", "decimal",
              "--excellon-oval-format", "alternate", "--excellon-separate-th", "-o", GERBERS, BOARD)
    with zipfile.ZipFile(OUT / "freedo-radio-gerbers.zip", "w", zipfile.ZIP_DEFLATED) as z:
        for f in sorted(GERBERS.iterdir()):
            z.write(f, f.name)


def parts():
    out = []
    for comp in ET.parse(WORK / "netlist.xml").getroot().iter("comp"):
        props = {p.get("name"): p.get("value", "") for p in comp.findall("property")}
        out.append({
            "ref": comp.get("ref"),
            "value": comp.findtext("value", ""),
            "footprint": comp.findtext("footprint", "").split(":")[-1],
            "mpn": props.get("MPN", ""),
            "lcsc": props.get("LCSC", ""),
            "asm": props.get("Asm", ""),
            "dnp": "dnp" in props,
            "in_bom": "exclude_from_bom" not in props,
        })
    return out


def write_csv(path, header, rows):
    with open(path, "w", newline="", encoding="utf-8") as f:
        w = csv.writer(f)
        w.writerow(header)
        w.writerows(rows)


def bom(jlc):
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
    write_csv(OUT / "jlc-bom.csv", ["Comment", "Designator", "Footprint", "LCSC Part #"], rows)
    return len(rows)


def read_csv(path):
    with open(path, newline="", encoding="utf-8") as f:
        return list(csv.DictReader(f))


def cpl(jlc):
    # One footprint can map to several EasyEDA lands (and rotations), so the key is the LCSC number too.
    corr = {(r["footprint"], lcsc): r for r in read_csv(ROTATIONS) for lcsc in r["lcsc"].split()}
    pos = {r["Ref"]: r for r in read_csv(WORK / "pos.csv")}
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
    write_csv(OUT / "jlc-cpl.csv", ["Designator", "Mid X", "Mid Y", "Layer", "Rotation"], rows)
    return len(rows)


def check_designators(jlc):
    want = {p["ref"] for p in jlc}
    written = {
        "jlc-bom.csv": [d for r in read_csv(OUT / "jlc-bom.csv") for d in r["Designator"].split(",")],
        "jlc-cpl.csv": [r["Designator"] for r in read_csv(OUT / "jlc-cpl.csv")],
    }
    for name, refs in written.items():
        if len(refs) != len(set(refs)) or set(refs) != want:
            sys.exit(f"{name} designators differ from the schematic Asm=JLC set: "
                     f"missing {sorted(want - set(refs))}, extra {sorted(set(refs) - want)}, "
                     f"{len(refs) - len(set(refs))} duplicates")


def hand_list(all_parts):
    groups = defaultdict(list)
    for p in all_parts:
        if p["asm"] == "hand" or p["dnp"]:
            groups[p["lcsc"] or p["ref"]].append(p)
    rows = []
    for ps in groups.values():
        n = len(ps)
        refs = ",".join(sorted((p["ref"] for p in ps), key=natural))
        note = "DNP, optional" if ps[0]["dnp"] else "fitted by hand"
        rows.append([refs, ps[0]["value"], ps[0]["mpn"] or "-", ps[0]["lcsc"] or "-", ps[0]["footprint"], note, n]
                    + [n * b for b in BOARD_COUNTS])
    rows.sort(key=lambda r: (r[5], natural(r[0].split(",")[0])))
    write_csv(OUT / "hand-solder.csv", ["Designator", "Value", "MPN", "LCSC Part #", "Footprint", "Note",
                                        "Per board"] + [f"Qty {b} boards" for b in BOARD_COUNTS], rows)
    return len(rows)


def main():
    export()
    all_parts = parts()
    jlc = [p for p in all_parts if p["asm"] == "JLC" and not p["dnp"]]
    bad = [p["ref"] for p in jlc if not p["lcsc"] or not p["in_bom"]]
    if bad:
        sys.exit(f"Asm=JLC parts without LCSC or excluded from the BOM: {bad}")
    lines = bom(jlc)
    placed = cpl(jlc)
    check_designators(jlc)
    hand = hand_list(all_parts)
    print(f"OK: {len(jlc)} JLC parts on {lines} BOM lines, {placed} CPL rows, {hand} hand-solder lines, "
          f"{len(list(GERBERS.iterdir()))} files in freedo-radio-gerbers.zip, output in {OUT}")


if __name__ == "__main__":
    main()
