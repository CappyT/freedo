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


@pytest.mark.parametrize("language", ("en", *LANGUAGES))
@pytest.mark.parametrize("key", ["hot_water_temperature_required", "zone_temperature_required"])
def test_entity_errors_are_translated(language, key):
    """The entities raise these keys when heat has no temperature to write."""
    exceptions = json.loads((INTEGRATION / f"{language}.json").read_text())["exceptions"]
    assert exceptions[key]["message"]


@pytest.mark.parametrize("language", LANGUAGES)
def test_addon_translation_matches_english(language):
    yaml = pytest.importorskip("yaml")
    source = yaml.safe_load((ADDON / "en.yaml").read_text())
    translated = yaml.safe_load((ADDON / f"{language}.yaml").read_text())
    assert differences(source, translated) == []


# The keys of the rooms feature. A tado zone is a room in these texts, and
# freedo, not the daemon, is the actor the user meets.
ROOM_KEYS = [
    ("options", "step", "init"),
    ("options", "step", "rooms"),
    ("options", "step", "room"),
    ("options", "step", "outdoor"),
    ("options", "error", "write_failed"),
    ("selector", "correction_method"),
    ("selector", "room_text"),
    ("entity", "select", "room_temperature_correction"),
    ("entity", "select", "temperature_source"),
    ("issues", "rooms_no_area"),
]
ZONE_WORD = {"en": "zone", "it": "zona", "de": "Zone", "fr": "zone"}
ROOMS_PATH = {
    "en": "Configure > Rooms",
    "it": "Configura > Stanze",
    "de": "Konfigurieren > Räume",
    "fr": "Configurer > Pièces",
}


def texts(node):
    if isinstance(node, dict):
        for value in node.values():
            yield from texts(value)
    else:
        yield node


@pytest.mark.parametrize("language", ("en", *LANGUAGES))
def test_room_texts_name_rooms_and_freedo(language):
    tree = json.loads((INTEGRATION / f"{language}.json").read_text())
    for path in ROOM_KEYS:
        node = tree
        for key in path:
            node = node[key]
        for text in texts(node):
            assert "daemon" not in text.lower(), (path, text)
            assert ZONE_WORD[language].lower() not in text.lower(), (path, text)
    # The repair issue points to the branch Rooms of Configure.
    assert ROOMS_PATH[language] in tree["issues"]["rooms_no_area"]["description"]
    # One name for the temperature source and one for the correction, in the
    # page of a room and on the select entities.
    room = tree["options"]["step"]["room"]
    select = tree["entity"]["select"]
    assert room["data"]["temperature_source"] == select["temperature_source"]["name"]
    assert (
        room["sections"]["advanced"]["data"]["correction"]
        == select["room_temperature_correction"]["name"]
    )


# An entity row on a device page leaves about 100 px to the name of a number,
# next to its input box, and about 160 px to a sensor name next to its state.
# These limits keep the names whole at 1280 px in the real frontend.
NAME_LIMITS = {"number": 15, "sensor": 24, "binary_sensor": 24}


@pytest.mark.parametrize("language", ("en", *LANGUAGES))
def test_entity_names_fit_a_device_page(language):
    entities = json.loads((INTEGRATION / f"{language}.json").read_text())["entity"]
    long = [
        (platform, key, name)
        for platform, limit in NAME_LIMITS.items()
        for key, value in entities[platform].items()
        if len(name := value["name"].replace("{circuit}", "1")) > limit
    ]
    assert long == []


DAEMON_WORD = {"en": "daemon", "it": "daemon", "de": "daemon", "fr": "démon"}
BARE_CELSIUS = re.compile(r"\d\s?C\b")


@pytest.mark.parametrize("language", ("en", *LANGUAGES))
def test_user_texts_say_room_and_freedo(language):
    """Every user text: a tado zone is a room, freedo is the actor (not the
    daemon), and a temperature carries the degree sign. Placeholders and keys
    are not user text."""
    tree = json.loads((INTEGRATION / f"{language}.json").read_text())
    problems = []
    for text in texts(tree):
        plain = PLACEHOLDER.sub("", text).lower()
        for word in (ZONE_WORD[language].lower(), DAEMON_WORD[language], "daemon"):
            if word in plain:
                problems.append((word, text))
        if BARE_CELSIUS.search(text):
            problems.append(("C", text))
    assert problems == []

