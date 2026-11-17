#!/usr/bin/env python3
"""Bounded production workflow verification; baseline automated testing is deferred."""
import argparse
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
    ap = argparse.ArgumentParser()
    ap.add_argument('--workflow', default='all')
    args = ap.parse_args()
    observations = []
    with tempfile.TemporaryDirectory(prefix='cadence-workflow-') as scratch:
        scratch = Path(scratch)
        ffmpeg = os.environ.get('FFMPEG_BIN', 'ffmpeg')
        subprocess.run([ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'error',
                        '-f', 'lavfi', '-i', 'testsrc2=size=160x90:rate=24:duration=4',
                        '-f', 'lavfi', '-i', 'sine=frequency=660:sample_rate=48000:duration=4',
                        '-c:v', 'libx264', '-preset', 'ultrafast', '-threads', '1',
                        '-c:a', 'aac', '-shortest', str(scratch/'calibration.mp4')], check=True, timeout=30)
        # Talk-like material: loud syllables separated by long pauses, one soft
        # word tail (quiet but audible), and one unmeasurable digital gap.
        talk_expr = ('0.25*sin(2*PI*440*t)*between(t,0,0.7)'
                     '+0.01*sin(2*PI*330*t)*between(t,0.7,1.0)'
                     '+0.25*sin(2*PI*520*t)*between(t,1.0,1.7)'
                     '+0.25*sin(2*PI*440*t)*between(t,2.4,3.1)'
                     '+0.25*sin(2*PI*600*t)*between(t,3.8,4.5)')
        subprocess.run([ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'error',
                        '-f', 'lavfi', '-i', 'testsrc2=size=160x90:rate=24:duration=4.5',
                        '-f', 'lavfi', '-i', f"aevalsrc=exprs='{talk_expr}':s=48000:d=4.5",
                        '-c:v', 'libx264', '-preset', 'ultrafast', '-threads', '1',
                        '-c:a', 'aac', '-shortest', str(scratch/'lecture.mp4')], check=True, timeout=30)
        with socket.socket() as sock:
            sock.bind(('127.0.0.1', 0))
            port = sock.getsockname()[1]
        address = 'http://127.0.0.1:'+str(port)+'/forge/v1'
        env = dict(os.environ, FORGE_ROOT=str(scratch/'data'),
                   FORGE_ADDR='127.0.0.1:'+str(port), FORGE_WORKERS='1')
        log = open(scratch/'server.log', 'ab')
        process = None
        def request(method, route, body=None, headers=None, status=200, raw=False):
            hs = dict(headers or {})
            if isinstance(body, (dict, list)):
                body = json.dumps(body).encode()
                hs['Content-Type'] = 'application/json'
            req = urllib.request.Request(address+route, data=body, headers=hs, method=method)
            try:
                response = urllib.request.urlopen(req, timeout=60)
            except urllib.error.HTTPError as exc:
                response = exc
            with response:
                data = response.read()
                need(response.status == status, f'{method} {route}: {response.status}, wanted {status}: {data[:600]!r}')
                return data if raw or not data else json.loads(data)
        def start():
            nonlocal process
            process = subprocess.Popen([str(ROOT/'bin/forged')], cwd=ROOT, env=env,
                                       stdout=log, stderr=log, start_new_session=True)
            deadline = time.monotonic()+10
            while time.monotonic() < deadline:
                try:
                    request('GET', '/health')
                    return
                except (OSError, RuntimeError):
                    if process.poll() is not None:
                        raise RuntimeError((scratch/'server.log').read_text())
                    time.sleep(.05)
            raise RuntimeError('server readiness timed out')
        def stop(force=False):
            nonlocal process
            if process and process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL if force else signal.SIGTERM)
                process.wait(timeout=15)
            process = None
        def observe(name, detail):
            observations.append({'workflow_id': name, 'passed': True, 'observed': detail})
        def wait_render(rid, desired=None):
            deadline = time.monotonic()+40
            while time.monotonic() < deadline:
                r = request('GET', '/renders/'+rid)
                if desired and r['state'] == desired:
                    return r
                if not desired and r['state'] in ('succeeded', 'failed', 'cancelled'):
                    return r
                time.sleep(.02)
            raise RuntimeError('render state timeout')
        def wait_analysis(sid, revision):
            deadline = time.monotonic()+40
            while time.monotonic() < deadline:
                a = request('GET', f'/sources/{sid}/silence/{revision}')
                if a['state'] in ('ready', 'failed'):
                    return a
                time.sleep(.02)
            raise RuntimeError('silence analysis timeout')
        try:
            start()
            media = (scratch/'calibration.mp4').read_bytes()
            source = request('POST', '/sources?name=calibration-pulse', media, status=201)
            sid = source['id']
            need(source['media']['video'] and source['media']['audio'], 'probe lost streams')
            need(source['digest'] == hashlib.sha256(media).hexdigest(), 'source digest mismatch')
            request('POST', '/sources?name=invalid', b'not-media', status=400)
            duplicate = request('POST', '/sources?name=duplicate', media, status=201)
            need(duplicate['id'] == sid, 'ingest did not converge')
            observe('ingest', 'MP4 accepted with SHA-256; malformed bytes rejected; duplicate converges')
            recipe_body = dict(name='review-proxy', format='mp4', width=160, height=90,
                               fps=24, sample_rate=48000, normalize=False, target_lufs=-16)
            recipe = request('POST', '/recipes', recipe_body, status=201)
            rp = recipe['id']
            updated = request('PUT', '/recipes/'+rp, dict(recipe_body, name='review-proxy-v2'), {'If-Match': '1'}, status=201)
            need(updated['revision'] == 2, 'recipe revision not advanced')
            request('PUT', '/recipes/'+rp, recipe_body, {'If-Match': '1'}, status=409)
            observe('recipe-version', 'revision 2 saved; stale revision rejected')
            sheet_body = dict(name='two-pass-calibration', spans=[
                dict(source_id=sid, in_ms=0, out_ms=1500, gain_db=-3),
                dict(source_id=sid, in_ms=2000, out_ms=3500, gain_db=0)],
                cues=[dict(source_id=sid, start_ms=1000, end_ms=2500, text='校准 <幅度>')])
            sheet = request('POST', '/sheets', sheet_body, status=201)
            sh = sheet['id']
            revision = request('PUT', '/sheets/'+sh, dict(sheet_body, name='revised-calibration'), {'If-Match': '1'}, status=201)
            need(revision['revision'] == 2, 'sheet revision not advanced')
            request('PUT', '/sheets/'+sh, sheet_body, {'If-Match': '1'}, status=409)
            observe('sheet-version', 'two-cut revision saved; stale writer rejected')
            plan = request('GET', '/sheets/'+sh+'/preview?recipe_id='+rp)
            need(plan['duration_ms'] == 3000 and plan['placements'][1]['output_in_ms'] == 1500, 'timeline mismatch')
            observe('preview', 'two source spans compacted to a 3000ms composition')
            need('00:00:01.000 --> 00:00:01.500' in plan['vtt'], 'first caption clipping mismatch')
            need('00:00:01.500 --> 00:00:02.000' in plan['vtt'], 'second caption mapping mismatch')
            need('&lt;幅度&gt;' in plan['vtt'], 'caption markup not escaped')
            observe('captions', 'source cue split and clipped into two escaped output intervals')
            if args.workflow in ('all', 'loudness'):
                loud = request('POST', '/sources/'+sid+'/loudness')
                need(loud['integrated_lufs'] is not None and not loud['silent'], 'loudness scan failed')
                observe('loudness', 'measured integrated LUFS and true peak')
            for mode in ['storyboard', 'waveform']:
                if args.workflow in ('all', mode):
                    image = request('GET', '/sources/'+sid+'/visual?mode='+mode, raw=True)
                    need(image.startswith(b'\x89PNG'), 'inspection output is not PNG')
                    observe(mode, f'PNG produced from source streams: {len(image)} bytes')
            lecture = request('POST', '/sources?name=lecture-pauses', (scratch/'lecture.mp4').read_bytes(), status=201)
            lid = lecture['id']
            first = None
            if args.workflow in ('all', 'silence-analysis'):
                requested = request('POST', '/sources/'+lid+'/silence', status=202)
                need(requested['state'] == 'pending' and requested['revision'] == 1, 'analysis was not accepted asynchronously')
                need(requested['source_digest'] == lecture['digest'], 'analysis is not bound to source bytes')
                # Editing remains possible while the scan runs.
                editing = request('PUT', '/sheets/'+sh, dict(sheet_body, name='edited-during-analysis'), {'If-Match': '2'}, status=201)
                need(editing['revision'] == 3, 'editing was blocked during analysis')
                first = wait_analysis(lid, 1)
                need(first['state'] == 'ready' and first['candidates'], 'analysis produced no reviewed intervals')
                need(all(c['source_id'] == lid for c in first['candidates']), 'candidate source drifted')
                kinds = {c['kind'] for c in first['candidates']}
                need('silence' in kinds, 'digital silence was not reported')
                need('soft_speech' in kinds, 'low-level speech was not reported as a tradeoff')
                silence_candidate = next(c for c in first['candidates'] if c['kind'] == 'silence')
                need(silence_candidate['suggested'] == 'cut', 'silence should be suggested for cutting')
                soft_candidate = next(c for c in first['candidates'] if c['kind'] == 'soft_speech')
                need(soft_candidate['suggested'] == 'tradeoff', 'soft speech was auto-approved')
                need(soft_candidate['peak_db'] is not None and soft_candidate['mean_rms_db'] is not None, 'soft speech lost its measured level')
                observe('silence-analysis', f'ready revision bound to SHA-256 reports {len(first["candidates"])} intervals; editing stayed available')
            hush = request('POST', '/sheets', dict(name='lecture-review', spans=[
                dict(source_id=lid, in_ms=0, out_ms=4500, gain_db=0)],
                cues=[dict(source_id=lid, start_ms=0, end_ms=1600, text='开场白'),
                      dict(source_id=lid, start_ms=1600, end_ms=2500, text='转折处'),
                      dict(source_id=lid, start_ms=3800, end_ms=4500, text='收尾')]), status=201)
            hid = hush['id']
            hush_revision = 1
            if first is None:
                request('POST', '/sources/'+lid+'/silence', status=202)
                first = wait_analysis(lid, 1)
                need(first['state'] == 'ready', 'analysis prerequisite failed')
            analysis_ref = dict(source_id=lid, revision=1, source_digest=lecture['digest'], source_bytes=lecture['bytes'])
            analyses = [analysis_ref]
            target_ms = 4100
            if args.workflow in ('all', 'silence-review'):
                options = dict(target_ms=target_ms, option='silence_only', analyses=analyses)
                proposal = request('POST', f'/sheets/{hid}/silence:propose?revision={hush_revision}', options)
                need(proposal['proposal']['feasible'], str(proposal['proposal'].get('problem')))
                need(proposal['proposal']['reached'], 'silence-only review should reach the target')
                need(proposal['proposal']['silence_cut_ms'] > 0, 'silence-only review removed nothing')
                need(proposal['proposal']['soft_speech_cut_ms'] == 0, 'silence-only review touched low-level speech')
                measured = request('GET', f'/sources/{lid}/silence/1')['candidates']
                decided = {d['candidate_id']: d for d in proposal['proposal']['decisions']}
                need(len(decided) == len(measured), 'review did not decide every measured interval')
                need(all(d['reason'] for d in decided.values()), 'a decision lacks its reason')
                need(any(d['action'] == 'keep' for d in decided.values()), 'review recorded no deliberate keeps')
                need(proposal['preview_ready'] and proposal['preview']['duration_ms'] <= target_ms, 'proposal preview is not usable')
                tied = [d for d in decided.values() if d['action'] == 'cut']
                need(tied and all(d['cuts'] for d in tied), 'a confirmed cut lacks exact source bytes')
                observe('silence-review', 'reviewable choices cover every measured interval, including deliberate keeps, with reasons and exact cut bytes')
            adopted = None
            if args.workflow in ('all', 'silence-adopt', 'silence-render'):
                # A later edit is confirmed while the reviewed proposal is open.
                later = request('PUT', '/sheets/'+hid, dict(name='lecture-review-confirmed', spans=[
                    dict(source_id=lid, in_ms=0, out_ms=4500, gain_db=0)],
                    cues=[dict(source_id=lid, start_ms=0, end_ms=1600, text='开场白'),
                          dict(source_id=lid, start_ms=1600, end_ms=2500, text='转折处'),
                          dict(source_id=lid, start_ms=3800, end_ms=4500, text='收尾')]), {'If-Match': str(hush_revision)}, status=201)
                hush_revision = later['revision']
                options = dict(target_ms=target_ms, option='silence_only', analyses=analyses)
                request('POST', f'/sheets/{hid}/silence:adopt', options, {'If-Match': '1'}, status=409)
                request('POST', f'/sheets/{hid}/silence:adopt', options, status=400)
                request('POST', f'/sheets/{hid}/silence:propose?revision={hush_revision}',
                        dict(target_ms=target_ms, option='silence_only', analyses=[]), status=400)
                adopted = request('POST', f'/sheets/{hid}/silence:adopt', options, {'If-Match': str(hush_revision)}, status=201)
                need(adopted['sheet']['revision'] == hush_revision+1, 'adoption did not append a revision')
                need(adopted['adoption']['base_revision'] == hush_revision, 'adoption base revision drifted')
                need(adopted['adoption']['analyses'][0]['source_digest'] == lecture['digest'], 'adoption lost source bytes')
                need(adopted['adoption']['result_revision'] == adopted['sheet']['revision'], 'adoption does not name its result revision')
                need(adopted['adoption']['cue_removed'] + adopted['adoption']['cue_clipped'] >= 1, 'adoption claims no caption impact')
                need(adopted['sheet']['silence_adoption'] == adopted['adoption']['id'], 'sheet revision does not carry its adoption')
                hush_revision = adopted['sheet']['revision']
                if args.workflow in ('all', 'silence-adopt'):
                    observe('silence-adopt', 'stale adoption refused at revision 1; current revision adopted with analysis, byte and caption lineage')
            if args.workflow in ('all', 'silence-reanalysis'):
                original = request('GET', f'/sources/{lid}/silence/1')
                again = request('POST', f'/sources/{lid}/silence', status=202)
                need(again['revision'] == 2, 're-analysis did not append a revision')
                second = wait_analysis(lid, 2)
                need(second['state'] == 'ready', 'second analysis failed')
                unchanged = request('GET', f'/sources/{lid}/silence/1')
                need(unchanged['state'] == 'ready' and unchanged['finished_at'] == original['finished_at'], 're-analysis rewrote the reviewed revision')
                request('POST', f'/sheets/{hid}/silence:propose?revision={hush_revision}', dict(target_ms=target_ms, option='silence_only', analyses=[dict(analysis_ref, revision=2)]))
                observe('silence-reanalysis', 're-analysis appends revision 2 without rewriting revision 1')
            if args.workflow in ('all', 'silence-tradeoff'):
                tight = request('POST', '/sheets', dict(name='lecture-tradeoff', spans=[
                    dict(source_id=lid, in_ms=0, out_ms=4500, gain_db=0)],
                    cues=[dict(source_id=lid, start_ms=0, end_ms=1600, text='开场白'),
                          dict(source_id=lid, start_ms=1600, end_ms=2500, text='转折处'),
                          dict(source_id=lid, start_ms=3800, end_ms=4500, text='收尾')]), status=201)
                tid = tight['id']
                hard_target = 3400
                review = request('POST', f'/sheets/{tid}/silence:propose?revision=1',
                                 dict(target_ms=hard_target, option='silence_only', analyses=analyses))
                # Pure silence alone cannot reach this target: the reviewer is told,
                # and the low-level interval stays kept rather than auto-cut.
                need(not review['proposal']['reached'], 'silence-only unexpectedly reached an unreachable target')
                need(review['proposal']['shortfall_ms'] > 0, 'shortfall was not reported')
                soft = next(c for c in review['proposal']['decisions'] if c['kind'] == 'soft_speech')
                need(soft['action'] == 'keep', 'soft word tail was cut while the option forbade it')
                options_seen = {c['option']: c for c in review['proposal']['choices']}
                need(set(options_seen) == {'keep_all', 'silence_only', 'tighten_pauses', 'silence_and_soft_tails'}, 'review did not offer every tradeoff')
                need(options_seen['silence_and_soft_tails']['soft_speech_cut_ms'] > 0, 'soft-tail option cut nothing')
                need(options_seen['silence_and_soft_tails']['result_ms'] < options_seen['silence_only']['result_ms'], 'soft-tail option is not a real tradeoff')
                need(any('shortfall' in w for w in review['proposal']['warnings']), 'review did not warn about the remaining shortfall')
                # Explicit review may reject the tradeoff and cut a smaller pause instead.
                silence_candidate = next(c for c in review['proposal']['decisions'] if c['kind'] == 'silence' and c['end_ms'] - c['start_ms'] > 400)
                custom = request('POST', f'/sheets/{tid}/silence:propose?revision=1', dict(
                    target_ms=hard_target, option='custom', analyses=analyses, decisions=[
                        dict(source_id=lid, candidate_id=silence_candidate['candidate_id'], action='cut', cut_ms=120),
                        dict(source_id=lid, candidate_id=soft['candidate_id'], action='keep')]))
                need(custom['proposal']['soft_speech_cut_ms'] == 0, 'custom review cut a kept interval')
                need(custom['proposal']['silence_cut_ms'] == 120, 'custom cut did not follow the reviewed amount')
                need(any(d['action'] == 'keep' and d['candidate_id'] == soft['candidate_id'] for d in custom['proposal']['decisions']), 'custom keep was not recorded')
                request('POST', f'/sheets/{tid}/silence:propose?revision=1', dict(
                    target_ms=hard_target, option='custom', analyses=analyses, decisions=[
                        dict(source_id=lid, candidate_id=silence_candidate['candidate_id'], action='cut', cut_ms=99999)]), status=400)
                request('POST', f'/sheets/{tid}/silence:adopt', dict(target_ms=hard_target, option='keep_all', analyses=analyses), {'If-Match': '1'}, status=400)
                traded = request('POST', f'/sheets/{tid}/silence:adopt', dict(target_ms=hard_target, option='silence_and_soft_tails', analyses=analyses), {'If-Match': '1'}, status=201)
                need(traded['adoption']['soft_speech_cut_ms'] > 0, 'adopted tradeoff did not record the soft-speech cut')
                need(not traded['adoption']['reached'] and traded['adoption']['shortfall_ms'] > 0, 'adopted tradeoff hid its shortfall')
                need(all(d['action'] == 'cut' for d in traded['adoption']['decisions'] if d['kind'] == 'soft_speech' and d['cut_ms'] > 0), 'soft cut lacks a decision')
                observe('silence-tradeoff', 'unreachable target stays reviewable: shortfall, all four options, soft-tail tradeoff and explicit custom keeps')
            if args.workflow in ('all', 'silence-render') and adopted is not None:
                out = request('POST', '/renders', dict(sheet_id=hid, sheet_revision=hush_revision, recipe_id=rp, recipe_revision=1),
                              {'Idempotency-Key': 'silence-render'}, status=202)
                final = wait_render(out['id'])
                need(final['state'] == 'succeeded', str(final['attempts']))
                need(final['silence_adoption']['id'] == adopted['adoption']['id'], 'render lost the adoption record')
                need(final['silence_adoption']['result_revision'] == hush_revision, 'render adoption revision drifted')
                need(final['silence_analyses'][0]['revision'] == 1 and final['silence_analyses'][0]['source_digest'] == lecture['digest'], 'render lost analysis byte lineage')
                request('POST', f"/renders/{out['id']}/verify")
                captions = request('GET', f"/renders/{out['id']}/artifact?captions=1", raw=True).decode()
                need('开场白' in captions and '收尾' in captions, 'delivered captions lost reviewed text')
                need(captions == final['plan']['vtt'], 'delivered captions drifted from the frozen plan')
                impacts = adopted['adoption']['cue_impacts']
                need(impacts, 'adoption recorded no caption impact for a cut that crosses caption text')
                need(all(i['removed_ms'] > 0 for i in impacts), 'caption impact lacks removed bytes')
                cut_spans = [c for d in adopted['adoption']['decisions'] if d['action'] == 'cut' for c in d['cuts']]
                need(cut_spans, 'adoption recorded no applied source intervals')
                need(all(s['span_index'] == 0 for s in cut_spans), 'applied cut names the wrong base span')
                need(final['plan']['duration_ms'] == final['sheet']['spans'][0]['out_ms'] - final['sheet']['spans'][0]['in_ms'] + sum(
                    s['out_ms'] - s['in_ms'] for s in final['sheet']['spans'][1:]), 'frozen plan does not describe the adopted spans')
                packed = request('GET', f"/renders/{out['id']}/bundle", raw=True)
                with zipfile.ZipFile(io.BytesIO(packed)) as z:
                    need('silence_trace.json' in z.namelist(), 'delivery bundle omitted the silence judgment trail')
                    trace = json.loads(z.read('silence_trace.json'))
                    need(trace['adoption']['id'] == adopted['adoption']['id'], 'bundle trace names a different adoption')
                    need(trace['analyses'][0]['source_digest'] == lecture['digest'], 'bundle trace lost analysis bytes')
                    need(trace['sheet_revision'] == hush_revision, 'bundle trace lost the adopted sheet revision')
                observe('silence-render', 'render freezes adoption and analysis bytes; delivered captions and bundle trace the adopted judgment')


            req = dict(sheet_id=sh, sheet_revision=1, recipe_id=rp, recipe_revision=1)
            render = request('POST', '/renders', req, {'Idempotency-Key': 'first-render'}, status=202)
            rid = render['id']
            again = request('POST', '/renders', req, {'Idempotency-Key': 'first-render'}, status=202)
            need(again['id'] == rid, 'idempotency key did not converge')
            request('POST', '/renders', dict(req, sheet_revision=2), {'Idempotency-Key': 'first-render'}, status=409)
            observe('idempotent-submit', 'same request converges; changed revision conflicts')
            done = wait_render(rid)
            need(done['state'] == 'succeeded', str(done['attempts']))
            need(done['sheet']['revision'] == 1 and done['recipe']['revision'] == 1, 'frozen revisions drifted')
            observe('mp4-render', 'frozen revisions encoded to H.264/AAC MP4 and inspected')
            verification = request('POST', '/renders/'+rid+'/verify')
            artifact = request('GET', '/renders/'+rid+'/artifact', raw=True)
            need(hashlib.sha256(artifact).hexdigest() == verification['digest'], 'artifact digest mismatch')
            observe('artifact-verify', 'download SHA-256 equals committed inspection')
            if args.workflow in ('all', 'bundle'):
                packed = request('GET', '/renders/'+rid+'/bundle', raw=True)
                with zipfile.ZipFile(io.BytesIO(packed)) as z:
                    need(set(z.namelist()) == {'composition.mp4', 'captions.vtt', 'composition.json'}, 'bundle members mismatch')
                    need(json.loads(z.read('composition.json')).get('silence_adoption') is None, 'plain render invented a silence adoption')
                    frozen = json.loads(z.read('composition.json'))
                    need(frozen['plan']['vtt'] == z.read('captions.vtt').decode(), 'bundle captions drifted')
                observe('bundle', 'verified media, captions and frozen manifest exported together')
            if args.workflow in ('all', 'wav-render'):
                ar = request('POST', '/recipes', dict(recipe_body, name='reference-audio', format='wav', normalize=True), status=201)
                audio_render = request('POST', '/renders', dict(req, recipe_id=ar['id']), {'Idempotency-Key': 'audio'}, status=202)
                audio_done = wait_render(audio_render['id'])
                need(audio_done['state'] == 'succeeded', str(audio_done['attempts']))
                need(audio_done['artifact']['inspection']['media']['audio_codec'] == 'pcm_s16le', 'audio codec mismatch')
                observe('wav-render', 'trimmed stereo PCM WAV encoded with loudness filter')
            request('DELETE', '/sources/'+sid, status=409)
            observe('retire-source', 'historical sheet reference prevents retirement')
            if args.workflow in ('all', 'cancel', 'retry', 'restart-recover'):
                long_sheet = request('POST', '/sheets', dict(sheet_body, name='extended-calibration', spans=sheet_body['spans']*16), status=201)
                pending = request('POST', '/renders', dict(req, sheet_id=long_sheet['id']), {'Idempotency-Key': 'interrupt'}, status=202)
                interrupted_id = pending['id']
                wait_render(interrupted_id, 'running')
                cancelled = request('POST', '/renders', req, {'Idempotency-Key': 'cancel-me'}, status=202)
                cancelled = request('POST', '/renders/'+cancelled['id']+'/cancel')
                need(cancelled['state'] == 'cancelled', 'cancel not acknowledged')
                request('GET', '/renders/'+cancelled['id']+'/artifact', status=409)
                observe('cancel', 'composition cancelled without artifact visibility')
                stop(force=True)
                start()
                recovered = request('GET', '/renders/'+interrupted_id)
                need(recovered['state'] == 'failed' and recovered['attempts'][-1]['outcome'] == 'interrupted', 'recovery provenance mismatch')
                observe('restart-recover', 'SIGKILL/restart retained frozen inputs and marked interruption')
                request('POST', '/renders/'+interrupted_id+'/retry', status=202)
                retried = wait_render(interrupted_id)
                need(retried['state'] == 'succeeded' and len(retried['attempts']) == 2, str(retried['attempts']))
                observe('retry', 'second attempt completed using original snapshot')
            if args.workflow in ('all', 'silence-interrupt'):
                # Use a longer source so the kill lands inside the scan rather
                # than racing a 4.5s analysis that may already be done.
                ffmpeg = os.environ.get('FFMPEG_BIN', 'ffmpeg')
                long_talk = ("0.25*sin(2*PI*440*t)*between(mod(t,2.0),0,0.9)"
                             "+0.25*sin(2*PI*520*t)*between(mod(t,2.0),1.3,1.9)"
                             "+0.008*sin(2*PI*300*t)*between(mod(t,2.0),0.9,1.1)")
                subprocess.run([ffmpeg, '-nostdin', '-hide_banner', '-loglevel', 'error',
                                '-f', 'lavfi', '-i', 'testsrc2=size=160x90:rate=12:duration=40',
                                '-f', 'lavfi', '-i', f"aevalsrc=exprs='{long_talk}':s=48000:d=40",
                                '-c:v', 'libx264', '-preset', 'ultrafast', '-threads', '1',
                                '-c:a', 'aac', '-shortest', str(scratch/'long-lecture.mp4')],
                               check=True, timeout=60)
                long_source = request('POST', '/sources?name=long-lecture',
                                      (scratch/'long-lecture.mp4').read_bytes(), status=201)
                long_id = long_source['id']
                history = request('GET', f'/sources/{long_id}/silence')
                expected_revision = len(history) + 1
                request('POST', f'/sources/{long_id}/silence', status=202)
                deadline = time.monotonic()+20
                while time.monotonic() < deadline:
                    running = request('GET', f'/sources/{long_id}/silence/{expected_revision}')
                    if running['state'] == 'running':
                        break
                    if running['state'] == 'ready':
                        raise RuntimeError('analysis finished before it could be interrupted')
                    time.sleep(.005)
                else:
                    raise RuntimeError('analysis never started')
                stop(force=True)
                start()
                interrupted = request('GET', f'/sources/{long_id}/silence/{expected_revision}')
                need(interrupted['state'] == 'failed', f'interrupted analysis is {interrupted["state"]}, not failed')
                need('restart' in interrupted['error'], 'interrupted analysis lacks an explicit reason')
                need(not interrupted['candidates'], 'interrupted analysis exposes partial candidates')
                request('POST', f'/sheets/{hid}/silence:propose?revision={hush_revision}',
                        dict(target_ms=target_ms, option='silence_only', analyses=analyses))
                observe('silence-interrupt', 'SIGKILL fails only the running revision; ready evidence and later edits stay usable')
            events = request('GET', '/events?after=0')
            need(events and all(a['sequence'] < b['sequence'] for a, b in zip(events, events[1:])), 'event order mismatch')
            tail = request('GET', '/events?after='+str(events[-1]['sequence']))
            need(all(e['sequence'] > events[-1]['sequence'] for e in tail), 'event cursor repeated')
            observe('events', 'persisted event cursor strictly increases')
            if args.workflow != 'all':
                need(any(x['workflow_id'] == args.workflow for x in observations), 'unknown workflow')
            print(json.dumps({'ok': True, 'requested': args.workflow, 'observations': observations}, ensure_ascii=False))
        except Exception:
            log.flush()
            print((scratch/'server.log').read_text(errors='replace'))
            raise
        finally:
            stop()
            log.close()

if __name__ == '__main__':
    main()
