"""Version identifiers that take part in the configuration hash.

Bump FILL_MODEL_VERSION whenever a fill, fee or accounting rule changes in a way that
can alter simulated trades. Runs made under different versions are not comparable.
"""

__version__ = "0.1.0"
FILL_MODEL_VERSION = "1"
