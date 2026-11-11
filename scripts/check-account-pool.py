"""Bounded live pool audit: two text requests and one image per enabled account.

Uses normal account rotation without changing account or runtime configuration.
Attributes a request only when exactly one matching new server log is observed.
Credentials stay in memory; artifacts are written under ignored data/.
"""
import base64
from collections import Counter
from datetime import datetime, timezone, timedelta
import io
import json
from pathlib import Path
import re
import sqlite3
import time

from PIL import Image
import requests

ROOT = Path(__file__).resolve().parents[1]
OUT = ROOT / 'data' / ('account-audit-' + datetime.now(timezone(timedelta(hours=8))).strftime('%Y%m%d-%H%M%S'))
OUT.mkdir()
DB = 'file:' + (ROOT / 'data/gemini.db').as_posix() + '?mode=ro'
cfg = json.loads((ROOT / 'data/local-config.json').read_text(encoding='utf-8-sig'))
BASE = 'http://127.0.0.1:' + str(cfg['port'])


def query(sql, args=()):
    with sqlite3.connect(DB, uri=True) as db:
        db.row_factory = sqlite3.Row
        return [dict(r) for r in db.execute(sql, args)]


def accounts():
    return query('select id,status,fail_count,proxy_id,pick_seq,last_ok_at from accounts order by id')


def save(name, value):
    (OUT / name).write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding='utf-8')


def emit(value):
    print(json.dumps(value, ensure_ascii=True), flush=True)


key = query("select v from kv where k='api_key'")[0]['v']
before = accounts()
enabled = [a['id'] for a in before if a['status'] == 'enabled']
if not enabled:
    raise SystemExit('No enabled accounts')
save('before.json', before)
runtime_before = query("select v from kv where k='runtime_config'")
results = []
started = time.monotonic()
stop_reason = None
emit({'output_dir': str(OUT), 'enabled': enabled, 'target_text_each': 2, 'target_images_each': 1})


def safe_error(value):
    return re.sub(r'https?://\S+', '[URL]', str(value).replace(key, '[REDACTED]'))[:600]


def run(kind, index):
    model = 'gemini-3.6-flash' if kind == 'text' else 'gemini-image'
    stream = kind == 'text' and index % 2 == 0
    tag = 'POOL_OK_' + str(index)
    path = '/v1/chat/completions' if kind == 'text' else '/v1/images/generations'
    if kind == 'text':
        payload = {'model': model, 'stream': stream, 'messages': [{'role': 'user', 'content': 'Reply exactly ' + tag}]}
    else:
        payload = {'model': model, 'n': 1, 'size': '1024x1024', 'response_format': 'b64_json',
                   'prompt': 'Generate one simple product photograph of a blue ceramic cup on a white table, no text. Variation ' + str(index)}
    last_id = query('select coalesce(max(id),0) as id from requests')[0]['id']
    picks = {a['id']: a['pick_seq'] for a in accounts()}
    row = {'index': index, 'kind': kind, 'model': model, 'stream': stream, 'ok': False}
    emit({'starting': index, 'kind': kind, 'stream': stream})
    t = time.monotonic()
    try:
        with requests.Session() as session:
            session.trust_env = False
            with session.post(BASE + path, headers={'Authorization': 'Bearer ' + key}, json=payload,
                              stream=True, timeout=(10, 240 if kind == 'image' else 150)) as response:
                row['status'] = response.status_code
                if response.status_code != 200:
                    obj = response.json()
                    err = obj.get('error', {})
                    row['error_code'] = err.get('code') if isinstance(err, dict) else None
                    row['error'] = safe_error(err)
                elif stream:
                    parts = []
                    row['done'] = False
                    for line in response.iter_lines(chunk_size=1024):
                        if time.monotonic() - t > 180:
                            raise TimeoutError('total stream limit')
                        if not line.startswith(b'data:'):
                            continue
                        data = line[5:].strip()
                        if data == b'[DONE]':
                            row['done'] = True
                            break
                        event = json.loads(data)
                        if event.get('error'):
                            row['error'] = safe_error(event['error'])
                        for choice in event.get('choices', []):
                            content = choice.get('delta', {}).get('content') or ''
                            if content and 'first_text_s' not in row:
                                row['first_text_s'] = round(time.monotonic() - t, 3)
                            parts.append(content)
                    row['text'] = ''.join(parts)
                    row['ok'] = row['done'] and tag in row['text'] and 'error' not in row
                elif kind == 'text':
                    obj = response.json()
                    row['text'] = ''.join(c.get('message', {}).get('content') or '' for c in obj.get('choices', []))
                    row['ok'] = tag in row['text']
                else:
                    obj = response.json()
                    images = []
                    for j, item in enumerate(obj.get('data', [])):
                        raw = base64.b64decode(item['b64_json'], validate=True)
                        image = Image.open(io.BytesIO(raw))
                        image.load()
                        filename = 'image-%02d-%d.%s' % (index, j, (image.format or 'png').lower())
                        (OUT / filename).write_bytes(raw)
                        images.append({'file': filename, 'width': image.width, 'height': image.height, 'bytes': len(raw)})
                    row['images'] = images
                    row['ok'] = bool(images)
    except Exception as exc:
        row['error_type'] = type(exc).__name__
    row['seconds'] = round(time.monotonic() - t, 3)
    time.sleep(0.3)
    logs = query('select id,model,upstream_model,account_id,status,total_ms,endpoint,stream from requests where id>? order by id', (last_id,))
    row['server_logs'] = logs
    if len(logs) == 1 and logs[0]['model'] == model and logs[0]['status'] == row.get('status'):
        row['account_id'] = logs[0]['account_id']
        row['upstream_model'] = logs[0]['upstream_model']
    else:
        row['attribution_ambiguous'] = True
    row['accounts_picked'] = [a['id'] for a in accounts() if a['pick_seq'] != picks.get(a['id'])]
    results.append(row)
    save('results.json', results)
    emit(row)
    return row


for kind, target in [('text', 2), ('image', 1)]:
    # At most one extra rotation if fallbacks or external traffic affect coverage.
    for _ in range(len(enabled) * (target + 1)):
        counts = Counter(r.get('account_id') for r in results if r['kind'] == kind)
        if all(counts[a] >= target for a in enabled):
            break
        if time.monotonic() - started > 1500:
            stop_reason = '25 minute total limit'
            break
        row = run(kind, len(results) + 1)
        if row.get('attribution_ambiguous'):
            stop_reason = 'ambiguous attribution or request not completed in server logs'
            break
        if row.get('status') in (401, 403) or (row.get('status') == 429 and row.get('error_code') != 'model_downgraded'):
            stop_reason = 'authentication, egress, or rate limit response'
            break
        time.sleep(2)
    if stop_reason:
        break

after = accounts()
save('after.json', after)
summary = {'seconds': round(time.monotonic() - started, 3), 'requests': len(results),
           'successful': sum(r['ok'] for r in results), 'stop_reason': stop_reason,
           'runtime_unchanged': runtime_before == query("select v from kv where k='runtime_config'"),
           'disabled_not_tested': [a['id'] for a in before if a['status'] != 'enabled'],
           'accounts': []}
for aid in enabled:
    rows = [r for r in results if r.get('account_id') == aid]
    summary['accounts'].append({'id': aid, 'text_attempts': sum(r['kind'] == 'text' for r in rows),
                                'text_ok': sum(r['kind'] == 'text' and r['ok'] for r in rows),
                                'image_attempts': sum(r['kind'] == 'image' for r in rows),
                                'image_ok': sum(r['kind'] == 'image' and r['ok'] for r in rows),
                                'status_after': next((a['status'] for a in after if a['id'] == aid), 'missing')})
save('summary.json', summary)
emit(summary)
