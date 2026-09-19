#!/usr/bin/env python3
"""sync-manifest: regenerate the schema-derived fields of commands.json from
the registry export (KS_MANIFEST_DUMP=r.json go test -run TestRegistryDump).

Only usage, summary, surface, aliases and flags are written, because those
are what the parser enforces. Everything the manifest states about MEASURED
behaviour (status, safety, safetyMeasured, budgetDefaults, note) is left
exactly as it is: a measurement is not regenerated from a schema. Key order
is preserved so the diff is the change and nothing else.
"""
import json, sys, collections
reg = json.load(open(sys.argv[1]))
p = 'commands.json'
m = json.load(open(p), object_pairs_hook=collections.OrderedDict)
byverb = {r['verb']: r for r in reg}
seen = set()
for row in m['commands']:
    r = byverb.get(row['verb'])
    if r is None:
        print(f"manifest row {row['verb']!r} has no registry command; remove it by hand", file=sys.stderr)
        sys.exit(1)
    seen.add(row['verb'])
    row['aliases'] = r['aliases']
    row['usage'] = r['usage']
    row['summary'] = r['summary']
    row['surface'] = r['surface']
    if r.get('flags'):
        row['flags'] = r['flags']
    elif 'flags' in row:
        del row['flags']
    bd = row.get('budgetDefaults')
    if bd and 'override' in bd:
        bd['override'] = 'ks run --budget-tokens N'
for r in reg:
    if r['verb'] not in seen:
        new = collections.OrderedDict([('verb', r['verb']), ('aliases', r['aliases']), ('usage', r['usage']),
                                       ('summary', r['summary']), ('status', 'available'), ('surface', r['surface'])])
        if r.get('flags'):
            new['flags'] = r['flags']
        m['commands'].append(new)
        print(f"added row for {r['verb']}", file=sys.stderr)
json.dump(m, open(p, 'w'), indent=1, ensure_ascii=False)
open(p, 'a').write('\n')
print(f"commands.json: {len(m['commands'])} rows synced from the registry")
