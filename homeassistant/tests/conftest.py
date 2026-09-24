"""Load the pure modules of the integration without Home Assistant.

The package __init__ imports Home Assistant, so the tests import api.py and
model.py as top-level modules.
"""

import pathlib
import sys

sys.path.insert(
    0, str(pathlib.Path(__file__).resolve().parents[1] / "custom_components" / "freedo")
)
