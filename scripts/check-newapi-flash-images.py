"""Eight bounded requests to the existing local WEB2API service, two at a time.

Reads local configuration/key without printing secrets or changing settings.
Checks Chat, Gemini and Images JSON/SSE; stores only sanitized results and images.
This is an upstream smoke test, not a running new-api deployment/load test.
"""
import argparse
import base64
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone, timedelta
import io
import json
from pathlib import Path
import sqlite3
import time

from PIL import Image
import requests

ROOT = Path(__file__).resolve().parents[1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--text-model', default='gemini-3.6-flash')
    parser.add_argument('--only', choices=('all', 'text', 'images'), default='all')
    args = parser.parse_args()
    config = json.loads((ROOT / 'data/local-config.json').read_text(encoding='utf-8-sig'))
    base = 'http://127.0.0.1:' + str(config['port'])
    with sqlite3.connect('file:' + (ROOT / 'data/gemini.db').as_posix() + '?mode=ro', uri=True) as db:
        key = db.execute("select v from kv where k='api_key'").fetchone()[0]
    try:
        with requests.Session() as session:
            session.trust_env = False
            health = session.get(base + '/', timeout=5)
            health.raise_for_status()
            version = health.json().get('version')
    except requests.RequestException as exc:
        print(json.dumps({'preflight': 'failed', 'error': type(exc).__name__, 'generation_requests': 0}), flush=True)
        return 2
    out = ROOT / 'data' / ('newapi-smoke-' + datetime.now(timezone(timedelta(hours=8))).strftime('%Y%m%d-%H%M%S'))
    out.mkdir()
    protocols = {'all': ('chat', 'gemini-text', 'images', 'gemini-image'),
                 'text': ('chat', 'gemini-text'), 'images': ('images', 'gemini-image')}[args.only]
    cases = [(protocol, stream) for protocol in protocols for stream in (False, True)]

    def run(case):
        protocol, stream = case
        is_image = protocol in ('images', 'gemini-image')
        model = 'gemini-image' if is_image else args.text_model
        prompt = 'Generate one photograph of a blue ceramic cup on a white table, no text.' if is_image else 'Reply exactly NEWAPI_OK'
        payload = {'model': model, 'stream': stream}
        if protocol == 'chat':
            path = '/v1/chat/completions'
            payload['messages'] = [{'role': 'user', 'content': prompt}]
            payload['stream_options'] = {'include_usage': True}
        elif protocol == 'images':
            path = '/v1/images/generations'
            payload.update(prompt=prompt, n=1, response_format='b64_json')
        else:
            action = 'streamGenerateContent?alt=sse' if stream else 'generateContent'
            path = '/v1beta/models/' + model + ':' + action
            payload = {'contents': [{'parts': [{'text': prompt}]}]}
        row = {'protocol': protocol, 'model': model, 'stream': stream, 'ok': False}
        started = time.monotonic()
        try:
            events = []
            done = False
            with requests.Session() as session:
                session.trust_env = False
                with session.post(base + path, headers={'Authorization': 'Bearer ' + key},
                                  json=payload, stream=True, timeout=(5, 240)) as response:
                    row['status'] = response.status_code
                    if response.status_code != 200:
                        row['error'] = 'HTTP ' + str(response.status_code)
                        return row
                    if stream:
                        for line in response.iter_lines(chunk_size=4096):
                            if time.monotonic() - started > 300:
                                raise TimeoutError('stream exceeded total limit')
                            if not line.startswith(b'data:'):
                                continue
                            data = line[5:].strip()
                            if data == b'[DONE]':
                                done = True
                                break
                            event = json.loads(data)
                            if event.get('error'):
                                raise ValueError('stream error')
                            row.setdefault('first_event_s', round(time.monotonic() - started, 3))
                            events.append(event)
                    else:
                        events = [response.json()]
            text, images = [], []
            finished = not stream or done
            usage_seen = False
            for event in events:
                usage_seen = usage_seen or 'usage' in event or 'usageMetadata' in event
                for choice in event.get('choices', []):
                    text.append((choice.get('delta') or choice.get('message') or {}).get('content') or '')
                for candidate in event.get('candidates', []):
                    finished = finished or candidate.get('finishReason') == 'STOP'
                    for part in candidate.get('content', {}).get('parts', []):
                        if not part.get('thought'):
                            text.append(part.get('text', ''))
                        if part.get('inlineData'):
                            images.append(part['inlineData']['data'])
                for item in event.get('data', []):
                    images.append(item['b64_json'])
                if event.get('type') == 'image_generation.completed':
                    images.append(event['b64_json'])
            row['finished'] = finished
            row['usage_seen'] = usage_seen
            row['dimensions'] = []
            for index, encoded in enumerate(images):
                raw = base64.b64decode(encoded, validate=True)
                with Image.open(io.BytesIO(raw)) as image:
                    image.load()
                    row['dimensions'].append(list(image.size))
                (out / f'{protocol}-{stream}-{index}.image').write_bytes(raw)
            row['ok'] = finished and (len(images) == 1 if is_image else 'NEWAPI_OK' in ''.join(text) and usage_seen)
        except Exception as exc:
            # Connection exceptions may contain URLs; retain only the class.
            row['error'] = type(exc).__name__
        finally:
            row['elapsed_s'] = round(time.monotonic() - started, 3)
            print(json.dumps(row), flush=True)
        return row

    print(json.dumps({'output_dir': str(out), 'service_version': version, 'requests': len(cases), 'concurrency': 2}), flush=True)
    with ThreadPoolExecutor(max_workers=2) as pool:
        results = list(pool.map(run, cases))
    summary = {'service_version': version, 'concurrency': 2, 'success': sum(r['ok'] for r in results), 'total': len(results), 'results': results}
    (out / 'results.json').write_text(json.dumps(summary, indent=2), encoding='utf-8')
    print(json.dumps({'success': summary['success'], 'total': len(results)}), flush=True)
    return 0 if all(r['ok'] for r in results) else 1


if __name__ == '__main__':
    raise SystemExit(main())
