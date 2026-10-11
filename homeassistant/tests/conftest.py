"""Load the pure modules of the integration without Home Assistant.

The package __init__ imports Home Assistant, so the unit tests import api.py and
model.py as top-level modules. The Home Assistant tests import the package as
custom_components.freedo.
"""

import pathlib
import sys

_ROOT = pathlib.Path(__file__).resolve().parents[2]
sys.path.insert(0, str(_ROOT / "custom_components" / "freedo"))
sys.path.append(str(_ROOT))
