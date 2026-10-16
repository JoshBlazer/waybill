"""Writes contracts.json: creation bytecode of Waybill's contracts, taken
from Foundry's build output, so Go integration tests can deploy the real
contracts on Anvil without Foundry installed.

Run through `make fixtures`; CI's contracts job fails if the committed file
differs from a fresh build (`make check-fixtures`).

Usage: python3 gen_fixture.py <contracts/out dir> <output file>
"""

import json
import sys

CONTRACTS = ["MockStablecoin", "Vault", "ForwarderFactory", "BatchPayout"]


def main(out_dir: str, dest: str) -> None:
    fixture = {}
    for name in CONTRACTS:
        with open(f"{out_dir}/{name}.sol/{name}.json") as f:
            artifact = json.load(f)
        fixture[name] = artifact["bytecode"]["object"]
    with open(dest, "w") as f:
        json.dump(fixture, f, indent=2, sort_keys=True)
        f.write("\n")


if __name__ == "__main__":
    main(sys.argv[1], sys.argv[2])
