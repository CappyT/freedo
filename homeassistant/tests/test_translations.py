"""The translations of the integration and of the add-on against their English
source: the same keys at every depth, and the same placeholders for each key.

The integration check needs no Home Assistant. The add-on check needs PyYAML,
which Home Assistant brings.
"""

import json
import pathlib
import re

import pytest

ROOT = pathlib.Path(__file__).resolve().parents[2]
INTEGRATION = ROOT / "custom_components" / "freedo" / "translations"
ADDON = ROOT / "addon" / "translations"
LANGUAGES = ("it", "de", "fr")
PLACEHOLDER = re.compile(r"\{(\w+)\}")


def nodes(node, path=()):
    """Every key path of a translation tree, with the value at its end."""
    found = {}
    if isinstance(node, dict):
        for key, value in node.items():
            found[path + (key,)] = value
            found.update(nodes(value, path + (key,)))
    return found


def differences(source, translated):
    """What a translation changes in the structure of its source: a missing or
    an extra key at any depth, a string where the source has a group or the
    other way round, and a different set of placeholders for a key."""
    a, b = nodes(source), nodes(translated)
    problems = [f"missing {'.'.join(p)}" for p in sorted(a.keys() - b.keys())]
    problems += [f"extra {'.'.join(p)}" for p in sorted(b.keys() - a.keys())]
    for path in sorted(a.keys() & b.keys()):
        name = ".".join(path)
        if isinstance(a[path], dict) != isinstance(b[path], dict):
            problems.append(f"{name} is a group in one file and a string in the other")
        elif isinstance(a[path], str):
            want, got = set(PLACEHOLDER.findall(a[path])), set(PLACEHOLDER.findall(str(b[path])))
            if want != got:
                problems.append(f"{name} has placeholders {sorted(got)}, the source has {sorted(want)}")
    return problems


def test_differences_finds_what_it_checks():
    source = {"a": {"b": "x {name}", "c": "y"}, "d": "z"}
    assert differences(source, json.loads(json.dumps(source))) == []
    assert differences(source, {"a": {"b": "x {name}"}, "d": "z"}) == ["missing a.c"]
    assert differences(source, {**source, "e": "w"}) == ["extra e"]
    assert differences(source, {"a": {**source["a"], "f": "w"}, "d": "z"}) == ["extra a.f"]
    assert differences(source, {"a": {"b": "x", "c": "y"}, "d": "z"}) == [
        "a.b has placeholders [], the source has ['name']"
    ]
    assert differences(source, {"a": {"b": "x {other}", "c": "y"}, "d": "z"}) == [
        "a.b has placeholders ['other'], the source has ['name']"
    ]
    assert differences(source, {"a": "x", "d": "z"}) == [
        "missing a.b",
        "missing a.c",
        "a is a group in one file and a string in the other",
    ]


@pytest.mark.parametrize("language", LANGUAGES)
def test_integration_translation_matches_english(language):
    source = json.loads((INTEGRATION / "en.json").read_text())
    translated = json.loads((INTEGRATION / f"{language}.json").read_text())
    assert differences(source, translated) == []


@pytest.mark.parametrize("language", LANGUAGES)
def test_addon_translation_matches_english(language):
    yaml = pytest.importorskip("yaml")
    source = yaml.safe_load((ADDON / "en.yaml").read_text())
    translated = yaml.safe_load((ADDON / f"{language}.yaml").read_text())
    assert differences(source, translated) == []
