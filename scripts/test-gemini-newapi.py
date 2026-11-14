"""Bounded Gemini/new-api live acceptance; no runtime configuration writes.

Set WEB2API_TEST_ADMIN in the process environment, then pass --base-url and
--out. Run beside the target service so the loopback fileData fixture is reachable.
Real generation consumes account quota. Reports contain only synthetic test data.
For Docker, bind the fixture to the project's bridge gateway with --fixture-host.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, HTTPServer


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--base-url', default=os.getenv('WEB2API_TEST_BASE', 'http://127.0.0.1:8083'))
    parser.add_argument('--out', default=os.getenv('WEB2API_TEST_OUT'), required=not os.getenv('WEB2API_TEST_OUT'))
    parser.add_argument('--reference', default=os.getenv('WEB2API_TEST_REFERENCE', str(Path(__file__).parent / 'testdata/gemini-reference.png')))
    parser.add_argument('--fixture-host', default=os.getenv('WEB2API_TEST_FIXTURE_HOST', '127.0.0.1'))
    args = parser.parse_args()
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    admin = os.environ['WEB2API_TEST_ADMIN']

    def admin_get(path):
        req = urllib.request.Request(args.base_url + path, headers={'Authorization': 'Bearer ' + admin})
        with opener.open(req, timeout=15) as response:
            return json.load(response)

    key = admin_get('/admin/api/apikey')['key']
    before_config = admin_get('/admin/api/config')['config']
    before_catalog = admin_get('/admin/api/models')['revision']
    reference = Path(args.reference).read_bytes()

    class FixtureHandler(BaseHTTPRequestHandler):
        def do_GET(self):
            if self.path == '/redirect':
                self.send_response(302)
                self.send_header('Location', '/reference.png')
                self.end_headers()
                return
            if self.path != '/reference.png':
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header('Content-Type', 'image/png')
            self.send_header('Content-Length', str(len(reference)))
            self.end_headers()
            self.wfile.write(reference)

        def log_message(self, *unused):
            pass

    fixture_server = HTTPServer((args.fixture_host, 0), FixtureHandler)
    fixture_thread = threading.Thread(target=fixture_server.serve_forever, daemon=True)
    fixture_thread.start()
    results = []

    def request(name, path, body=None, expected_status=200, stream=False, marker=None, image_count=None, image_only=False, auth=True):
        headers = {'x-goog-api-key': key} if auth else {}
        data = None
        if body is not None:
            headers['Content-Type'] = 'application/json'
            data = json.dumps(body).encode()
        row = {'name': name, 'path': path, 'ok': False}
        started = time.monotonic()
        objects = []
        try:
            req = urllib.request.Request(args.base_url + path, headers=headers, data=data)
            try:
                response = opener.open(req, timeout=210)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                row['status'] = response.code
                row['content_type'] = response.headers.get('Content-Type', '')
                if stream and response.code == 200 and 'text/event-stream' in row['content_type']:
                    for line in response:
                        if not line.startswith(b'data:'):
                            continue
                        event = json.loads(line[5:].strip())
                        objects.append(event)
                else:
                    obj = json.load(response)
                    objects = obj if isinstance(obj, list) else [obj]
            (out / (name + '.json')).write_text(json.dumps(objects, ensure_ascii=False), encoding='utf-8')
            assert row['status'] == expected_status, 'unexpected HTTP status'
            if expected_status != 200:
                err = objects[0]['error']
                assert err['code'] == expected_status and err.get('status'), 'non-Gemini error envelope'
            elif path.endswith('/models'):
                models = objects[0]['models']
                assert models and all(isinstance(m['name'], str) and m['name'].startswith('models/') for m in models)
                row['model_ids'] = [m['name'].removeprefix('models/') for m in models]
                assert 'gemini-image' in row['model_ids'] and 'gemini-4-hydrogen' in row['model_ids']
            elif ':countTokens' in path:
                assert objects[0]['totalTokens'] > 0
            elif ':' not in path:
                assert objects[0]['name'].startswith('models/')
            else:
                assert objects and all('error' not in obj for obj in objects)
                parts = [p for obj in objects for c in obj['candidates'] for p in c['content']['parts']]
                text = ''.join(p.get('text', '') for p in parts if not p.get('thought'))
                row['text_preview'] = text[:250]
                if marker:
                    assert all(word.lower() in text.lower() for word in marker), 'missing expected text/image content'
                images = [p['inlineData'] for p in parts if 'inlineData' in p]
                row['image_files'] = []
                for i, item in enumerate(images):
                    raw = base64.b64decode(item['data'], validate=True)
                    assert item['mimeType'].startswith('image/') and (raw.startswith(b'\x89PNG') or raw.startswith(b'\xff\xd8'))
                    filename = name + '-' + str(i) + '.bin'
                    (out / filename).write_bytes(raw)
                    row['image_files'].append(filename)
                if image_count is not None:
                    assert len(images) == image_count, 'image missing or duplicated'
                if image_only:
                    assert not text and all('inlineData' in p for p in parts), 'IMAGE-only response leaked text'
                final = objects[-1]
                assert final['candidates'][0]['finishReason'] == 'STOP'
                assert final.get('modelVersion') and final.get('responseId')
                assert all(final['usageMetadata'].get(k, -1) >= 0 for k in ['promptTokenCount', 'candidatesTokenCount', 'totalTokenCount'])
            row['ok'] = True
        except Exception as error:
            row['error'] = str(error).replace(key, '[REDACTED]').replace(admin, '[REDACTED]')[:300]
        row['seconds'] = round(time.monotonic() - started, 3)
        results.append(row)
        (out / 'results.json').write_text(json.dumps(results, ensure_ascii=False, indent=2), encoding='utf-8')
        print(json.dumps(row, ensure_ascii=True), flush=True)
        return objects if row['ok'] else None

    text_model = '/v1beta/models/gemini-4-hydrogen'
    image_model = '/v1beta/models/gemini-image'
    inline = {'inlineData': {'mimeType': 'image/png', 'data': base64.b64encode(reference).decode()}}
    vision_prompt = {'text': 'Describe the two large colored shapes in the image in English. Include each color and shape.'}
    try:
        request('models', '/v1beta/models')
        request('model-detail', text_model)
        request('auth-error', '/v1beta/models', expected_status=401, auth=False)
        request('bad-modality', text_model + ':generateContent', {'contents': [{'parts': [{'text': 'draw'}]}], 'generationConfig': {'responseModalities': ['IMAGE']}}, expected_status=400)
        request('count-tokens', text_model + ':countTokens', {'contents': [{'parts': [{'text': 'hello world'}]}]})
        for name, action, stream in [('text-json', ':generateContent', False), ('text-sse', ':streamGenerateContent?alt=sse', True), ('text-array', ':streamGenerateContent', False)]:
            request(name, text_model + action, {'contents': [{'role': 'user', 'parts': [{'text': 'Reply exactly GEMINI_NATIVE_OK'}]}]}, stream=stream, marker=['GEMINI_NATIVE_OK'])
        request('vision-inline', text_model + ':generateContent', {'contents': [{'role': 'user', 'parts': [inline, vision_prompt]}]}, marker=['red', 'square', 'blue', 'circle'])
        request('vision-model-history', text_model + ':generateContent', {'contents': [{'role': 'model', 'parts': [inline]}, {'role': 'user', 'parts': [vision_prompt]}]}, marker=['red', 'square', 'blue', 'circle'])
        request('vision-file-redirect', text_model + ':generateContent', {'contents': [{'parts': [{'fileData': {'mimeType': 'image/png', 'fileUri': 'http://' + args.fixture_host + ':' + str(fixture_server.server_port) + '/redirect'}}, vision_prompt]}]}, marker=['red', 'square', 'blue', 'circle'])
        config = {'responseModalities': ['IMAGE'], 'imageConfig': {'aspectRatio': '1:1', 'imageSize': '1K'}}
        generated = request('image-json', image_model + ':generateContent', {'contents': [{'parts': [{'text': 'Generate a studio product photo of a red toy car on a white table, centered, no text.'}]}], 'generationConfig': config}, image_count=1, image_only=True)
        request('image-sse', image_model + ':streamGenerateContent?alt=sse', {'contents': [{'parts': [{'text': 'Generate a photo of a blue ceramic cup on a white table, centered, no text.'}]}], 'generationConfig': {'responseModalities': ['TEXT', 'IMAGE'], 'imageConfig': {'aspectRatio': '1:1', 'imageSize': '1K'}}}, stream=True, image_count=1)
        if generated:
            ref = next(p for p in generated[0]['candidates'][0]['content']['parts'] if 'inlineData' in p)
            request('edit-inline', image_model + ':generateContent', {'contents': [{'role': 'user', 'parts': [ref, {'text': 'Edit the attached photo: change only the red car paint to blue, keep the same toy car, angle, wheels and white background.'}]}], 'generationConfig': config}, image_count=1, image_only=True)
            request('edit-model-history-sse', image_model + ':streamGenerateContent?alt=sse', {'contents': [{'role': 'user', 'parts': [{'text': 'Generate a red toy car photo.'}]}, {'role': 'model', 'parts': [ref]}, {'role': 'user', 'parts': [{'text': 'Change only the car paint to green. Keep the same car, angle, wheels and white background.'}]}], 'generationConfig': config}, stream=True, image_count=1, image_only=True)
    finally:
        fixture_server.shutdown()
        fixture_server.server_close()
        summary = {'results': results, 'success': sum(r['ok'] for r in results), 'total': len(results), 'config_unchanged': before_config == admin_get('/admin/api/config')['config'], 'catalog_unchanged': before_catalog == admin_get('/admin/api/models')['revision']}
        (out / 'summary.json').write_text(json.dumps(summary, ensure_ascii=False, indent=2), encoding='utf-8')
        print(json.dumps({k: v for k, v in summary.items() if k != 'results'}), flush=True)
    if not all(r['ok'] for r in results) or len(results) != 15:
        raise SystemExit(1)


if __name__ == '__main__':
    main()
