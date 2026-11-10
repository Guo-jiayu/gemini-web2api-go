"""50 real generation requests; no service/config changes or client retries.

Credentials stay in memory. Defaults to the local runtime database API key.
"""
import argparse
import base64
import concurrent.futures as cf
import hashlib
import io
import json
import math
import os
from pathlib import Path
import re
import sqlite3
import threading
import time
from datetime import datetime, timezone, timedelta

import requests
from PIL import Image

ROOT = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser()
parser.add_argument('--base-url')
parser.add_argument('--key-file')
parser.add_argument('--workers', type=int, default=3)
parser.add_argument('--interval', type=float, default=2.5)
parser.add_argument('--dry-run', action='store_true')
parser.add_argument('--start-id', type=int, default=1)
args = parser.parse_args()
ratios = ['1:1', '16:9', '9:16', '4:3', '3:4', '3:2', '2:3', '4:5', '5:4', '21:9']
cases = []
for index in range(25):
    for kind in ['text', 'image']:
        ident = len(cases) + 1
        api = ['chat', 'responses', 'gemini'][index % 3]
        case = dict(id=ident, kind=kind, api=api, stream=bool(index % 2),
                    model=('gemini-3.6-flash' if index < 15 else 'gemini-3.1-pro') if kind == 'text' else 'gemini-image')
        if kind == 'image':
            case.update(ratio=ratios[index % 10], image_size='1K' if index < 15 else '2K' if index < 20 else '4K')
        cases.append(case)
cases = [case for case in cases if case['id'] >= args.start_id]
if args.dry_run:
    print(json.dumps(cases, indent=2))
    raise SystemExit

cfg = json.loads((ROOT/'data/local-config.json').read_text(encoding='utf-8-sig'))
base = (args.base_url or 'http://127.0.0.1:' + str(cfg['port'])).rstrip('/')
if args.key_file:
    key = Path(args.key_file).read_text(encoding='utf-8-sig').strip()
elif os.environ.get('WEB2API_API_KEY'):
    key = os.environ['WEB2API_API_KEY']
else:
    with sqlite3.connect('file:'+(ROOT/'data/gemini.db').as_posix()+'?mode=ro', uri=True) as db:
        key = db.execute("select v from kv where k='api_key'").fetchone()[0]

def session():
    s = requests.Session()
    s.trust_env = False
    s.headers['Authorization'] = 'Bearer ' + key
    return s

# A failed preflight is not counted as a generation request.
with session() as s:
    response = s.get(base+'/v1/models', timeout=15)
    response.raise_for_status()
    available = [m['id'] for m in response.json().get('data', [])]
missing = sorted({c['model'] for c in cases} - set(available))
if missing:
    raise SystemExit('Missing requested models: '+repr(missing))

OUT = ROOT/'data'/('live-test-50-'+datetime.now(timezone(timedelta(hours=8))).strftime('%Y%m%d-%H%M%S'))
(OUT/'images').mkdir(parents=True)
lock = threading.Lock()
rate_lock = threading.Lock()
next_start = 0.0
results = []
start = time.monotonic()
run_tag = OUT.name[-6:]

def save(name, value):
    (OUT/name).write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding='utf-8')

def safe_error(value):
    value = str(value).replace(key, '[REDACTED]')
    return re.sub(r'https?://[^\s\"<>]+', '[URL]', value)[:1000]

save('run.json', dict(base_url=base, cases=cases, workers=args.workers, interval=args.interval,
                      started=datetime.now(timezone(timedelta(hours=8))).isoformat(), client_retries=0))
print(json.dumps({'started': str(OUT), 'requests': len(cases)}), flush=True)

def run(case):
    global next_start
    with rate_lock:
        time.sleep(max(0, next_start-time.monotonic()))
        next_start = time.monotonic()+args.interval
    row = dict(case, images=[], done=False, first_content_s=None)
    marker = f'TEST_{run_tag}_{case["id"]:03d}_END'
    if case['kind'] == 'text':
        prompt = f'请先原样输出 {marker}，再用中文解释 API 并发的一个优点，控制在 80 字以内。不要输出其他测试标识。'
    else:
        subject = ['a red ceramic teapot', 'a blue toy sailboat', 'a yellow origami crane', 'a green ceramic vase', 'an orange toy bicycle'][(case['id']//2-1) % 5]
        row['subject'] = subject
        prompt = f'Generate one studio product photograph of {subject} on a light gray table, centered composition, soft natural lighting, no text or watermark.'
    row['prompt'] = prompt
    if case['api'] == 'gemini':
        action = 'streamGenerateContent?alt=sse' if case['stream'] else 'generateContent'
        endpoint = '/v1beta/models/'+case['model']+':'+action
        payload = {'contents': [{'role': 'user', 'parts': [{'text': prompt}]}]}
        if case['kind'] == 'image':
            payload['generationConfig'] = {'imageConfig': {'imageSize': case['image_size'], 'aspectRatio': case['ratio']}}
    else:
        endpoint = '/v1/responses' if case['api'] == 'responses' else '/v1/chat/completions'
        payload = {'model': case['model'], 'stream': case['stream']}
        if case['api'] == 'responses':
            payload['input'] = prompt
        else:
            payload['messages'] = [{'role': 'user', 'content': prompt}]
        if case['kind'] == 'image':
            payload.update(image_size=case['image_size'], aspect_ratio=case['ratio'])
    begun = time.monotonic()
    row['start_offset'] = round(begun-start, 3)
    parts, encoded = [], []

    def consume(obj):
        if obj.get('error'):
            row['error'] = safe_error(obj['error'])
        if obj.get('type') in ['response.failed', 'error']:
            row['error'] = safe_error(obj.get('response', {}).get('error') or obj.get('error') or obj.get('type'))
        new = []
        for choice in obj.get('choices', []):
            new.append((choice.get('delta') or choice.get('message') or {}).get('content') or '')
        if obj.get('type') == 'response.output_text.delta':
            new.append(obj.get('delta', ''))
        if obj.get('type') == 'response.completed':
            row['done'] = True
        if not case['stream']:
            for item in obj.get('output', []):
                new.extend(p.get('text', '') for p in item.get('content', []))
        for candidate in obj.get('candidates', []):
            if candidate.get('finishReason'):
                row['done'] = True
            for part in candidate.get('content', {}).get('parts', []):
                if not part.get('thought'):
                    new.append(part.get('text', ''))
                image = part.get('inlineData') or part.get('inline_data')
                if image:
                    encoded.append(image['data'])
        if (any(new) or encoded) and row['first_content_s'] is None:
            row['first_content_s'] = round(time.monotonic()-begun, 3)
        parts.extend(new)

    try:
        with session() as s, s.post(base+endpoint, json=payload, stream=case['stream'], timeout=(10, 300)) as response:
            row['http_status'] = response.status_code
            if response.status_code != 200:
                row['error'] = safe_error(response.json().get('error', 'HTTP '+str(response.status_code)))
            elif case['stream']:
                for line in response.iter_lines(chunk_size=8192):
                    if not line.startswith(b'data:'):
                        continue
                    data = line[5:].strip()
                    if data == b'[DONE]':
                        row['done'] = True
                        break
                    consume(json.loads(data))
            else:
                consume(response.json())
                row['done'] = True
        text = ''.join(parts)
        if case['kind'] == 'text':
            row['text'] = text
            row['artifact_ok'] = marker in text and all(m == marker for m in re.findall(r'TEST_\d+_\d+_END', text))
        else:
            encoded.extend(re.findall(r'data:image/[a-zA-Z0-9.+-]+;base64,([A-Za-z0-9+/=]+)', text))
            seen = set()
            for b64 in encoded:
                raw = base64.b64decode(b64, validate=True)
                digest = hashlib.sha256(raw).hexdigest()
                if digest in seen:
                    continue
                seen.add(digest)
                with Image.open(io.BytesIO(raw)) as im:
                    im.load()
                    width, height = im.size
                    ext = {'PNG': 'png', 'JPEG': 'jpg', 'WEBP': 'webp'}.get(im.format, 'img')
                path = OUT/'images'/f'{case["id"]:03d}-{len(seen)}.{ext}'
                path.write_bytes(raw)
                rw, rh = map(int, case['ratio'].split(':'))
                row['images'].append(dict(file=str(path), width=width, height=height, sha256=digest,
                                          bytes=len(raw), ratio_ok=abs((width/height)/(rw/rh)-1)<=.02))
            row['artifact_ok'] = bool(row['images']) and all(i['ratio_ok'] for i in row['images'])
            if not encoded:
                row['text_preview'] = text[:300]
        row['ok'] = row.get('http_status') == 200 and row['done'] and row['artifact_ok'] and 'error' not in row
    except Exception as exc:
        row.update(ok=False, error=safe_error(type(exc).__name__+': '+str(exc)))
    row['total_s'] = round(time.monotonic()-begun, 3)
    with lock:
        results.append(row)
        with (OUT/'requests.jsonl').open('a', encoding='utf-8') as f:
            f.write(json.dumps(row, ensure_ascii=False)+'\n')
        print(json.dumps({k: row.get(k) for k in ['id', 'kind', 'api', 'stream', 'ok', 'http_status', 'total_s', 'error']}), flush=True)
    return row

with cf.ThreadPoolExecutor(max_workers=args.workers) as pool:
    list(pool.map(run, cases))

def stats(rows):
    values = sorted(r['total_s'] for r in rows if r['ok'])
    return dict(total=len(rows), success=sum(r['ok'] for r in rows),
                p50_s=values[math.ceil(len(values)*.5)-1] if values else None,
                p95_s=values[math.ceil(len(values)*.95)-1] if values else None,
                min_s=min(values) if values else None, max_s=max(values) if values else None)

summary = dict(overall=stats(results), elapsed_s=round(time.monotonic()-start, 3),
               by_model={m: stats([r for r in results if r['model'] == m]) for m in sorted({r['model'] for r in results})},
               by_api={a: stats([r for r in results if r['api'] == a]) for a in ['chat', 'responses', 'gemini']},
               failures=[{k:r.get(k) for k in ['id', 'model', 'api', 'stream', 'http_status', 'error', 'total_s', 'text_preview']} for r in results if not r['ok']],
               image_files=sum(len(r['images']) for r in results))
save('summary.json', summary)
print(json.dumps({'summary': summary, 'output': str(OUT)}, ensure_ascii=True), flush=True)
