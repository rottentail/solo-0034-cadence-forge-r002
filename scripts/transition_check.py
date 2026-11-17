#!/usr/bin/env python3
"""End-to-end verification for production overlap transitions.

Exercises the real HTTP API and real FFmpeg processes:
  * requested overlaps are preserved when the spans can fund them;
  * short middle spans trigger a reviewable, recorded clamp instead of a
    silent shrink or a rejected edit;
  * dialogue cues and key-visual markers keep their frames and speech out of
    the shared dissolve window;
  * repeated source spans and silent video both survive the mix;
  * the preview plan, the rendered MP4 and the frozen bundle describe the same
    source-to-output relationship, including after a failed-attempt retry.
"""
import hashlib
import io
import json
import os
import signal
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def need(value, message):
    if not value:
        raise RuntimeError(message)


def main():
    observations = []
    with tempfile.TemporaryDirectory(prefix='cadence-transition-') as scratch:
        scratch = Path(scratch)
        ffmpeg = os.environ.get('FFMPEG_BIN', 'ffmpeg')
        ffprobe = os.environ.get('FFPROBE_BIN', 'ffprobe')

        def make_clip(name, color, freq, duration, silent=False):
            dest = scratch / name
            args = [ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'error',
                    '-f', 'lavfi', '-i', f'color=c={color}:s=160x90:r=20:duration={duration}',
                    '-f', 'lavfi', '-i', f'sine=frequency={freq}:sample_rate=48000:duration={duration}']
            if silent:
                args = [ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'error',
                        '-f', 'lavfi', '-i', f'color=c={color}:s=160x90:r=20:duration={duration}']
            args += ['-c:v', 'libx264', '-preset', 'ultrafast', '-threads', '1',
                     '-pix_fmt', 'yuv420p']
            if not silent:
                args += ['-c:a', 'aac', '-shortest']
            args += [str(dest)]
            subprocess.run(args, check=True, timeout=60)
            return dest

        red = make_clip('red.mp4', 'red', 300, 3.0)
        green = make_clip('green.mp4', 'green', 600, 3.0)
        blue = make_clip('blue.mp4', 'blue', 900, 3.0)
        yellow = make_clip('yellow-silent.mp4', 'yellow', 0, 2.0, silent=True)

        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        address = 'http://127.0.0.1:' + str(port) + '/forge/v1'
        env = dict(os.environ, FORGE_ROOT=str(scratch / 'data'),
                   FORGE_ADDR='127.0.0.1:' + str(port), FORGE_WORKERS='1')
        log = open(scratch / 'server.log', 'ab')
        process = None

        def request(method, route, body=None, headers=None, status=200, raw=False):
            hs = dict(headers or {})
            if isinstance(body, (dict, list)):
                body = json.dumps(body).encode()
                hs['Content-Type'] = 'application/json'
            req = urllib.request.Request(address + route, data=body, headers=hs, method=method)
            try:
                response = urllib.request.urlopen(req, timeout=60)
            except urllib.error.HTTPError as exc:
                response = exc
            with response:
                data = response.read()
                need(response.status == status,
                     f'{method} {route}: {response.status}, wanted {status}: {data[:600]!r}')
                return data if raw or not data else json.loads(data)

        def start():
            nonlocal process
            process = subprocess.Popen([str(ROOT / 'bin' / 'forged')], cwd=ROOT, env=env,
                                       stdout=log, stderr=log, start_new_session=True)
            deadline = time.monotonic() + 10
            while time.monotonic() < deadline:
                try:
                    request('GET', '/health')
                    return
                except (OSError, RuntimeError):
                    if process.poll() is not None:
                        raise RuntimeError((scratch / 'server.log').read_text())
                    time.sleep(.05)
            raise RuntimeError('server readiness timed out')

        def stop(force=False):
            nonlocal process
            if process and process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL if force else signal.SIGTERM)
                process.wait(timeout=15)
            process = None

        def wait_render(rid):
            deadline = time.monotonic() + 60
            while time.monotonic() < deadline:
                r = request('GET', '/renders/' + rid)
                if r['state'] in ('succeeded', 'failed', 'cancelled'):
                    return r
                time.sleep(.05)
            raise RuntimeError('render state timeout')

        def observe(name, detail):
            observations.append({'workflow_id': name, 'passed': True, 'observed': detail})

        def probe(path):
            out = subprocess.run([ffprobe, '-v', 'error', '-show_format', '-show_streams',
                                  '-of', 'json', str(path)], check=True, capture_output=True, text=True)
            return json.loads(out.stdout)

        def frame_rgb(path, at_seconds):
            png = scratch / f'frame-{at_seconds:.3f}.png'
            subprocess.run([ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'error',
                            '-ss', f'{at_seconds:.3f}', '-i', str(path), '-frames:v', '1', str(png)],
                           check=True, timeout=30)
            # Decode the single PNG frame through ffmpeg rawvideo to avoid an
            # image-library dependency.
            raw = scratch / f'frame-{at_seconds:.3f}.rgb'
            subprocess.run([ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'error',
                            '-i', str(png), '-pix_fmt', 'rgb24', '-f', 'rawvideo', str(raw)],
                           check=True, timeout=30)
            data = raw.read_bytes()
            return data[0], data[1], data[2]

        try:
            start()

            def ingest(path, name):
                return request('POST', '/sources?name=' + name, path.read_bytes(), status=201)

            sid_red = ingest(red, 'red')['id']
            sid_green = ingest(green, 'green')['id']
            sid_blue = ingest(blue, 'blue')['id']
            sid_yellow = ingest(yellow, 'yellow-silent')['id']
            need(request('GET', '/state')['sources'][sid_yellow]['media']['video'] is True,
                 'silent source lost its video stream')
            observe('ingest', 'four real clips ingested, including a video-only source')

            recipe = request('POST', '/recipes', dict(name='delivery', format='mp4', width=160,
                                                       height=90, fps=20, sample_rate=48000,
                                                       normalize=False, target_lufs=-16), status=201)
            rp = recipe['id']

            # --- Case A: requested overlaps fully affordable ----------------
            # red(2s) x500 green(2s) x500 green-repeat(1s) x300 yellow(2s).
            # No protected content touches the dissolve windows, so every
            # requested overlap survives unchanged.
            sheet_a = dict(name='overlap-happy-path', spans=[
                dict(source_id=sid_red, in_ms=0, out_ms=2000, gain_db=0),
                dict(source_id=sid_green, in_ms=0, out_ms=2000, gain_db=0),
                dict(source_id=sid_green, in_ms=2000, out_ms=3000, gain_db=-3),
                dict(source_id=sid_yellow, in_ms=0, out_ms=2000, gain_db=0)],
                transitions=[dict(before_index=1, requested_ms=500),
                             dict(before_index=2, requested_ms=500),
                             dict(before_index=3, requested_ms=300)])
            sheet_a_resp = request('POST', '/sheets', sheet_a, status=201)
            sh_a = sheet_a_resp['id']
            plan_a = request('GET', '/sheets/' + sh_a + '/preview?recipe_id=' + rp)
            need(plan_a['duration_ms'] == 7000 - 1300, 'happy-path duration mismatch')
            need([t['effective_ms'] for t in plan_a['transitions']] == [500, 500, 300],
                 'affordable overlaps were not preserved')
            need(all(not t['reasons'] for t in plan_a['transitions']), 'spurious clamp reasons')
            # Placement offsets: 0, 1500, 3000, 3700.
            need([p['output_in_ms'] for p in plan_a['placements']] == [0, 1500, 3000, 3700],
                 'placement offsets ignore overlaps')

            # The same composition with a cue inside the repeated span must
            # keep the dialogue whole: the entering dissolve is pulled back
            # until it clears the cue, and the mapped VTT interval never
            # overlaps a dissolve window.
            sheet_a_cue = dict(sheet_a, name='overlap-with-dialogue',
                               cues=[dict(source_id=sid_green, start_ms=2200, end_ms=2600,
                                          text='重复片段对白')])
            sh_ac = request('POST', '/sheets', sheet_a_cue, status=201)['id']
            plan_ac = request('GET', '/sheets/' + sh_ac + '/preview?recipe_id=' + rp)
            t_ac = plan_ac['transitions'][1]
            need(t_ac['effective_ms'] == 200 and any(x['code'] == 'protected_content' for x in t_ac['reasons']),
                  'repeated-span dialogue did not pull back the dissolve')
            need('00:00:03.500 --> 00:00:03.900' in plan_ac['vtt'], 'repeated-span cue mapping wrong')
            observe('transition-plan', 'requested overlaps preserved when affordable; dialogue clamps are explained')

            render_a = request('POST', '/renders', dict(sheet_id=sh_a, sheet_revision=1,
                                                        recipe_id=rp, recipe_revision=1),
                               {'Idempotency-Key': 'case-a'}, status=202)
            done_a = wait_render(render_a['id'])
            need(done_a['state'] == 'succeeded', str(done_a.get('attempts')))
            mp4_a = scratch / 'case-a.mp4'
            mp4_a.write_bytes(request('GET', '/renders/' + render_a['id'] + '/artifact', raw=True))
            media_a = probe(mp4_a)
            need(media_a['streams'][0]['codec_name'] == 'h264', 'video codec mismatch')
            need(abs(float(media_a['format']['duration']) - 5.7) <= 0.12, 'rendered duration diverges from plan')
            # The first dissolve spans output 1.5..2.0s: sample its midpoint
            # and both edges to prove a real progressive crossfade rather than
            # a cut. Midpoint red and green contributions must both be present.
            r0, g0, _ = frame_rgb(mp4_a, 1.55)
            rm, gm, _ = frame_rgb(mp4_a, 1.75)
            r1, g1, _ = frame_rgb(mp4_a, 1.95)
            if not (g0 < g1 and r0 > r1 and gm > 30 and rm > 30):
                raise RuntimeError(f'debug fade edges ({r0},{g0}) mid({rm},{gm}) ({r1},{g1})')
            need(g0 < g1 and r0 > r1 and gm > 30 and rm > 30, 'xfade is not a progressive red->green blend')
            # Inside the pure green repeated span (the second dissolve ends at
            # 3.5s; sample 3.6s) the frame is green. yuv420p lowers full-saturation
            # green to roughly 127, so assert channel ordering rather than 255.
            r, g, b = frame_rgb(mp4_a, 3.6)
            if not (g > 100 and r < 60 and b < 60):
                raise RuntimeError(f'debug green rgb=({r},{g},{b})')
            need(g > 100 and r < 60 and b < 60, 'repeated green span frame lost')
            # Silent yellow span still shows its picture (sample 5.2s, clear of
            # the 3.7..4.0s dissolve) and carries an audio stream.
            r, g, b = frame_rgb(mp4_a, 5.2)
            if not (r > 120 and g > 120 and b < 110):
                raise RuntimeError(f'debug yellow rgb=({r},{g},{b})')
            need(r > 120 and g > 120 and b < 110, 'silent yellow picture was not retained')
            audio_streams = [s for s in media_a['streams'] if s['codec_type'] == 'audio']
            need(len(audio_streams) == 1, 'silent source did not get a continuous audio bed')
            # Audio evidence for acrossfade: red carries 300Hz, green 600Hz.
            # Quantitative check via astats RMS: the dissolve midpoint is an
            # audible mix, while the synthesized silent span stays near mute.
            def rms_db(path, start, length):
                out = subprocess.run([ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'info',
                                      '-ss', str(start), '-t', str(length), '-i', str(path),
                                      '-af', 'astats=metadata=1:reset=0', '-f', 'null', '-'],
                                     capture_output=True, text=True, timeout=30)
                levels = [line for line in out.stderr.splitlines() if 'RMS level dB' in line]
                need(levels, 'astats produced no RMS measurement')
                return float(levels[-1].split(']')[-1].split(':')[-1])
            mix_level = rms_db(mp4_a, 1.75, 0.2)
            silent_level = rms_db(mp4_a, 5.3, 0.2)
            need(mix_level > -45, f'dissolve midpoint unexpectedly quiet: {mix_level}dB')
            need(silent_level < -80, f'silent yellow span was not silent: {silent_level}dB')
            observe('transition-render', 'xfade blend, acrossfade audio, repeated span and silent picture all verified')

            # --- Case B: short middle span contention -----------------------
            # red 1s, green 300ms, blue 1s; requests 400ms on each side. The
            # green span cannot fund 800ms of overlap; the second (leaving)
            # boundary must be reduced with a recorded contention reason.
            sheet_b = dict(name='short-middle', spans=[
                dict(source_id=sid_red, in_ms=0, out_ms=1000, gain_db=0),
                dict(source_id=sid_green, in_ms=0, out_ms=300, gain_db=0),
                dict(source_id=sid_blue, in_ms=0, out_ms=1000, gain_db=0)],
                transitions=[dict(before_index=1, requested_ms=400),
                             dict(before_index=2, requested_ms=400)])
            sh_b = request('POST', '/sheets', sheet_b, status=201)['id']
            plan_b = request('GET', '/sheets/' + sh_b + '/preview?recipe_id=' + rp)
            t1, t2 = plan_b['transitions']
            # The 300ms middle span keeps at least one frame of exclusive
            # material, so its entering overlap caps at 299ms.
            need(t1['effective_ms'] == 299, f'first overlap should use full short span, got {t1}')
            need(t2['effective_ms'] < 400 and t2['effective_ms'] >= 0, 'leaving overlap not clamped')
            need(any(x['code'] in ('contention_clamped', 'span_too_short') for x in t2['reasons']),
                  'clamp was not made reviewable')
            # Both overlaps together can never exceed the middle span.
            need(t1['effective_ms'] + t2['effective_ms'] <= 300, 'middle span was overspent by overlaps')
            observe('contention', f'short span clamp recorded: 400ms -> {t2["effective_ms"]}ms with reason')

            # --- Case C: protected dialogue and key visual -------------------
            # Marker at green 150ms and a cue covering 600..1100ms protect the
            # green head from the requested 500ms dissolve.
            sheet_c = dict(name='protected', spans=[
                dict(source_id=sid_red, in_ms=0, out_ms=2000, gain_db=0),
                dict(source_id=sid_green, in_ms=0, out_ms=2000, gain_db=0)],
                transitions=[dict(before_index=1, requested_ms=500)],
                markers=[dict(source_id=sid_green, at_ms=150, label='关键画面')],
                cues=[dict(source_id=sid_green, start_ms=600, end_ms=1100, text='不能盖住的对白')])
            sh_c = request('POST', '/sheets', sheet_c, status=201)['id']
            plan_c = request('GET', '/sheets/' + sh_c + '/preview?recipe_id=' + rp)
            tc = plan_c['transitions'][0]
            need(tc['effective_ms'] == 150, f'marker should cap overlap at 150ms, got {tc}')
            need(any(x['code'] == 'protected_content' for x in tc['reasons']), 'protection reason missing')
            # Green starts at 2000-150=1850ms, so the cue maps to
            # 2450..2950ms, entirely after the 1700..1850 dissolve.
            need('00:00:02.450 --> 00:00:02.950' in plan_c['vtt'], 'protected cue shifted into dissolve')
            observe('protection', 'key visual capped overlap at 150ms and dialogue stayed fully outside the window')

            # A marker at the very head (at_ms=0) forces the overlap to zero
            # rather than dropping the frame: legal edit, explicit reason.
            sheet_d = dict(sheet_c, name='protected-zero',
                           markers=[dict(source_id=sid_green, at_ms=0, label='首帧')])
            sh_d = request('POST', '/sheets', sheet_d, status=201)['id']
            plan_d = request('GET', '/sheets/' + sh_d + '/preview?recipe_id=' + rp)
            need(plan_d['transitions'][0]['effective_ms'] == 0, 'head marker did not force zero overlap')
            need(plan_d['transitions'][0]['reasons'], 'zero-overlap protection not explained')
            observe('zero-overlap', 'first-frame key visual produced an explained zero overlap, not a rejection')

            # --- Case E: freeze survives later edits and a failed retry ------
            frozen = request('POST', '/renders', dict(sheet_id=sh_c, sheet_revision=1,
                                                       recipe_id=rp, recipe_revision=1),
                             {'Idempotency-Key': 'freeze-c'}, status=202)
            frozen_done = wait_render(frozen['id'])
            need(frozen_done['state'] == 'succeeded', str(frozen_done.get('attempts')))
            frozen_plan = frozen_done['plan']
            # Caption team revises source cues while editors keep working: a
            # new sheet revision must not change the already-frozen render.
            revised = request('PUT', '/sheets/' + sh_c,
                              dict(sheet_c, name='protected-v2',
                                   cues=[dict(source_id=sid_green, start_ms=700, end_ms=1200,
                                              text='修订后的对白')]),
                              {'If-Match': '1'}, status=201)
            need(revised['revision'] == 2, 'sheet revision did not advance')
            check = request('GET', '/renders/' + frozen['id'])
            need(check['plan'] == frozen_plan, 'frozen render drifted after sheet revision')
            need(check['sheet']['cues'][0]['text'] == '不能盖住的对白', 'frozen sheet content drifted')
            observe('freeze', 'post-submit caption revision left the confirmed plan and sheet snapshot unchanged')

            # A failure followed by retry must reproduce the same confirmed
            # relationship. Force a real attempt failure through the public
            # restart path: SIGKILL the process while the render is running,
            # then explicitly retry the interrupted task.
            heavy_spans = (sheet_a['spans'] * 8)[:32]
            heavy_transitions = [dict(before_index=k, requested_ms=200) for k in range(1, len(heavy_spans))]
            heavy = request('POST', '/sheets', dict(sheet_a, name='heavy-repeat',
                                                    spans=heavy_spans,
                                                    transitions=heavy_transitions), status=201)
            heavy_plan = request('GET', '/sheets/' + heavy['id'] + '/preview?recipe_id=' + rp)
            interrupted = request('POST', '/renders',
                                  dict(sheet_id=heavy['id'], sheet_revision=1,
                                       recipe_id=rp, recipe_revision=1),
                                  {'Idempotency-Key': 'interrupt-transition'}, status=202)
            deadline = time.monotonic() + 20
            while time.monotonic() < deadline:
                if request('GET', '/renders/' + interrupted['id'])['state'] == 'running':
                    break
                time.sleep(.05)
            stop(force=True)
            start()
            recovered = request('GET', '/renders/' + interrupted['id'])
            need(recovered['state'] == 'failed' and recovered['attempts'][-1]['outcome'] == 'interrupted',
                  'interrupted render was not recovered as failed')
            need(recovered['plan']['transitions'] == heavy_plan['transitions'],
                  'interruption altered the frozen transition relationship')
            request('POST', '/renders/' + interrupted['id'] + '/retry', status=202)
            retried = wait_render(interrupted['id'])
            need(retried['state'] == 'succeeded' and len(retried['attempts']) == 2, str(retried['attempts']))
            need(retried['plan'] == recovered['plan'],
                  'retry did not reuse the confirmed plan')
            observe('retry', 'interrupted transition render retried into the identical frozen relationship')

            # --- Bundle carries one confirmed version ------------------------
            packed = request('GET', '/renders/' + frozen['id'] + '/bundle', raw=True)
            with zipfile.ZipFile(io.BytesIO(packed)) as z:
                names = set(z.namelist())
                need(names == {'composition.mp4', 'captions.vtt', 'composition.json'}, 'bundle members mismatch')
                manifest = json.loads(z.read('composition.json'))
                need(manifest['plan'] == frozen_plan, 'bundle plan differs from confirmed render')
                need(manifest['plan']['vtt'] == z.read('captions.vtt').decode(), 'bundle captions drifted')
                need(manifest['plan']['transitions'][0]['effective_ms'] == 150, 'bundle lost transition decision')
            observe('bundle', 'ZIP manifest, captions and media share the single confirmed transition plan')

            print(json.dumps({'ok': True, 'observations': observations}, ensure_ascii=False))
        except Exception:
            log.flush()
            print((scratch / 'server.log').read_text(errors='replace'))
            raise
        finally:
            stop()
            log.close()


if __name__ == '__main__':
    import signal  # noqa: E402
    main()
