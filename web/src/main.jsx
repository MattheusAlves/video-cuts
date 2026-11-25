import React, { useEffect, useMemo, useState } from 'react';
import { createRoot } from 'react-dom/client';
import './style.css';

const fmt = (seconds = 0) => {
  const mins = Math.floor(seconds / 60);
  const secs = Math.floor(seconds % 60);
  return `${mins}:${String(secs).padStart(2, '0')}`;
};
const stages = [
  { id: 'download', label: 'Get the video', description: 'YouTube source, audio and picture' },
  { id: 'transcribe', label: 'Transcribe', description: 'Local speech-to-text' },
  { id: 'select', label: 'Find the story', description: 'Hook, attention, setup and payoff' },
  { id: 'render', label: 'Render your cut', description: 'Captions and chosen frame' },
  { id: 'finalize', label: 'Save for review', description: 'Preparing the review queue' },
];

function elapsed(job) {
  if (!job.started_at) return '';
  const end = job.finished_at ? Date.parse(job.finished_at) : Date.now();
  return `${fmt(Math.max(0, Math.floor((end - Date.parse(job.started_at)) / 1000)))} elapsed`;
}

function App() {
  const [config, setConfig] = useState(null);
  const [mode, setMode] = useState('short');
  const [inputType, setInputType] = useState('library');
  const [source, setSource] = useState('');
  const [videos, setVideos] = useState([]);
  const [videoID, setVideoID] = useState('');
  const [url, setURL] = useState('');
  const [minSeconds, setMinSeconds] = useState(45);
  const [maxSeconds, setMaxSeconds] = useState(60);
  const [resolution, setResolution] = useState('1080x1920');
  const [framing, setFraming] = useState('fit_blur');
  const [captions, setCaptions] = useState(true);
  const [jobs, setJobs] = useState([]);
  const [clips, setClips] = useState([]);
  const [loadingVideos, setLoadingVideos] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');

  const activeJob = jobs.find((job) => job.status === 'running') || jobs[0];
  const stageIndex = stages.findIndex((stage) => stage.id === activeJob?.progress?.stage);
  const currentVideo = useMemo(() => videos.find((video) => video.id === videoID), [videos, videoID]);

  async function api(path, options) {
    const response = await fetch(path, options);
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(body.error || `Request failed (${response.status})`);
    return body;
  }

  async function refreshClips() {
    try { setClips(await api('/api/clips')); } catch (e) { setError(e.message); }
  }

  async function refreshJobs() {
    try {
      const next = await api('/api/jobs');
      setJobs(next.sort((a, b) => new Date(b.created_at) - new Date(a.created_at)));
      if (next.some((job) => ['done', 'failed'].includes(job.status))) refreshClips();
    } catch (e) { setError(e.message); }
  }

  useEffect(() => {
    api('/api/config').then((data) => {
      setConfig(data);
      setSource(data.sources?.[0]?.id || '');
      setMinSeconds(data.short.min_seconds || 45);
      setMaxSeconds(data.short.max_seconds || 60);
      setCaptions(Boolean(data.captions_enabled));
    }).catch((e) => setError(e.message));
    refreshClips();
    refreshJobs();
    const timer = setInterval(() => { refreshJobs(); refreshClips(); }, 5000);
    return () => clearInterval(timer);
  }, []);

  useEffect(() => {
    if (!config) return;
    const values = config[mode];
    setMinSeconds(values.min_seconds);
    setMaxSeconds(values.max_seconds);
    setResolution(mode === 'short' ? '1080x1920' : '1920x1080');
  }, [mode, config]);

  useEffect(() => {
    if (!source || inputType !== 'library') return;
    setLoadingVideos(true);
    setVideos([]);
    setVideoID('');
    api(`/api/videos?source=${encodeURIComponent(source)}`)
      .then((items) => { setVideos(items); setVideoID(items[0]?.id || ''); })
      .catch((e) => setError(e.message))
      .finally(() => setLoadingVideos(false));
  }, [source, inputType]);

  async function createJob(event) {
    event.preventDefault();
    setError('');
    setSubmitting(true);
    const request = {
      mode, min_seconds: Number(minSeconds), max_seconds: Number(maxSeconds),
      resolution, framing, captions_enabled: captions,
    };
    if (inputType === 'url') request.url = url.trim();
    else { request.source_id = source; request.video_id = videoID; }
    try {
      const job = await api('/api/jobs', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(request),
      });
      setJobs((current) => [job, ...current]);
    } catch (e) { setError(e.message); }
    finally { setSubmitting(false); }
  }

  if (!config) return <main className="boot">Opening Cutroom…</main>;

  return <main className="shell">
    <header className="topbar">
      <a className="brand" href="#top"><span className="brand-mark">C</span><span>cutroom<small>YOUTUBE VIDEO STUDIO</small></span></a>
      <div className="local-badge"><i /> LOCAL WORKSPACE</div>
    </header>

    <section className="intro" id="top">
      <div className="eyebrow">YOUR EDITING DESK <span>·</span> 01 / CREATE</div>
      <h1>Find the moment.<br /><em>Make it unmissable.</em></h1>
      <p>Choose a source, set the cut, and let the pipeline find a story worth staying for.</p>
    </section>

    {error && <div className="notice error"><span>{error}</span><button onClick={() => setError('')} aria-label="Dismiss">×</button></div>}

    <div className="workspace">
      <section className="panel create-panel">
        <div className="panel-heading"><div><span className="step">01</span><h2>New cut</h2></div><span className="required">YOUTUBE SOURCE</span></div>
        <form onSubmit={createJob}>
          <div className="field-label">WHERE SHOULD WE LOOK?</div>
          <div className="segmented source-toggle">
            <button type="button" className={inputType === 'library' ? 'active' : ''} onClick={() => setInputType('library')}>Channel library</button>
            <button type="button" className={inputType === 'url' ? 'active' : ''} onClick={() => setInputType('url')}>Paste YouTube link</button>
          </div>
          {inputType === 'library' ? <div className="source-fields">
            <label className="field"><span>CHANNEL</span><select value={source} onChange={(e) => setSource(e.target.value)}>
              {config.sources.map((item) => <option key={item.id} value={item.id}>{item.id}</option>)}
            </select></label>
            <label className="field"><span>VIDEO</span><select value={videoID} onChange={(e) => setVideoID(e.target.value)} disabled={loadingVideos || videos.length === 0}>
              {loadingVideos && <option>Loading recent uploads…</option>}
              {!loadingVideos && videos.length === 0 && <option value="">No videos found</option>}
              {videos.map((item) => <option key={item.id} value={item.id}>{item.title} · {fmt(item.duration)}</option>)}
            </select></label>
            {currentVideo && <a className="source-link" href={currentVideo.webpage_url} target="_blank" rel="noreferrer">Open source video ↗</a>}
          </div> : <label className="field url-field"><span>YOUTUBE VIDEO URL</span><input value={url} onChange={(e) => setURL(e.target.value)} placeholder="https://www.youtube.com/watch?v=…" required /></label>}

          <div className="field-label mode-label">WHAT ARE WE MAKING?</div>
          <div className="mode-cards">
            <button type="button" className={`mode-card ${mode === 'short' ? 'selected' : ''}`} onClick={() => setMode('short')}>
              <span className="mode-icon phone">▯</span><span><b>Short</b><small>Vertical · under 3 min</small></span><span className="radio-dot" />
            </button>
            <button type="button" className={`mode-card ${mode === 'video' ? 'selected' : ''}`} onClick={() => setMode('video')}>
              <span className="mode-icon screen">▭</span><span><b>Regular video</b><small>Landscape · up to 28 min</small></span><span className="radio-dot" />
            </button>
          </div>

          <div className="field-label duration-heading">CUT SETTINGS <span>{mode === 'short' ? 'IN SECONDS' : 'IN MINUTES'}</span></div>
          <div className="settings-grid">
            <label className="field"><span>MIN LENGTH</span><div className="number-wrap"><input type="number" min={mode === 'short' ? 1 : 1} max={mode === 'short' ? 179 : Math.floor(maxSeconds / 60)} step="1" value={mode === 'short' ? minSeconds : Math.floor(minSeconds / 60)} onChange={(e) => setMinSeconds(Number(e.target.value) * (mode === 'short' ? 1 : 60))} /><b>{mode === 'short' ? 'sec' : 'min'}</b></div></label>
            <label className="field"><span>MAX LENGTH</span><div className="number-wrap"><input type="number" min={mode === 'short' ? minSeconds : Math.ceil(minSeconds / 60)} max={mode === 'short' ? 179 : 28} step="1" value={mode === 'short' ? maxSeconds : Math.floor(maxSeconds / 60)} onChange={(e) => setMaxSeconds(Number(e.target.value) * (mode === 'short' ? 1 : 60))} /><b>{mode === 'short' ? 'sec' : 'min'}</b></div></label>
            <label className="field"><span>FRAME</span><select value={resolution} onChange={(e) => setResolution(e.target.value)}><option value="1080x1920">Vertical · 9:16</option><option value="1920x1080">Landscape · 16:9</option></select></label>
            <label className="field"><span>FILL</span><select value={framing} onChange={(e) => setFraming(e.target.value)}><option value="fit_blur">Fit with blurred sides</option><option value="center_crop">Crop to frame</option></select></label>
          </div>
          <label className="caption-setting"><span className="caption-icon">Aa</span><span><b>Burn in captions</b><small>Word-level subtitles with a highlighted hook</small></span><input type="checkbox" checked={captions} onChange={(e) => setCaptions(e.target.checked)} /><i /></label>
          <button className="submit" type="submit" disabled={submitting || (inputType === 'library' && !videoID) || (inputType === 'url' && !url.trim())}>
            {submitting ? 'Starting pipeline…' : <>Generate cut <span>↗</span></>}
          </button>
          <p className="form-footnote">The selector looks for a strong opening hook, a complete story, and a clear payoff.</p>
        </form>
      </section>

      <aside className="side-column">
        <section className="panel pipeline-panel"><div className="panel-heading"><div><span className="step">02</span><h2>Pipeline</h2></div><span className="live-pill"><i /> {activeJob?.status === 'running' ? 'RUNNING' : 'READY'}</span></div>
          {activeJob && <p className="pipeline-current" aria-live="polite">{activeJob.message} {elapsed(activeJob) && <span>· {elapsed(activeJob)}</span>}</p>}
          <ol className="pipeline-list">{stages.map((stage, index) => {
            const state = activeJob?.status === 'done' || (stageIndex >= 0 && index < stageIndex) ? 'complete' : index === stageIndex && activeJob?.status === 'running' ? 'current' : index === stageIndex && activeJob?.status === 'failed' ? 'failed' : '';
            return <li className={state} key={stage.id}><span>{state === 'complete' ? '✓' : String(index + 1).padStart(2, '0')}</span><div><b>{stage.label}</b><small>{state === 'current' ? activeJob.progress.message : stage.description}</small>{state === 'current' && stage.id === 'select' && activeJob.progress.total > 0 && <progress max={activeJob.progress.total} value={activeJob.progress.completed || 0} aria-label="Transcript windows analyzed" />}</div></li>;
          })}</ol>
        </section>
        <section className="panel jobs-panel"><div className="panel-heading"><div><span className="step">03</span><h2>Recent runs</h2></div><button className="icon-button" onClick={refreshJobs} aria-label="Refresh jobs">↻</button></div>
          {jobs.length === 0 ? <p className="empty">Your runs will appear here.</p> : <div className="job-list">{jobs.slice(0, 5).map((job) => <div className="job" key={job.id}><div className={`job-indicator ${job.status}`} /><div className="job-copy"><b>{job.request.url || job.request.video_id || 'YouTube video'}</b><small>{job.request.mode === 'video' ? 'Regular video' : 'Short'} · {job.message || job.status}{elapsed(job) ? ` · ${elapsed(job)}` : ''}</small>{job.error && <small className="job-error">{job.error}</small>}</div><span className={`status ${job.status}`}>{job.status}</span></div>)}</div>}
        </section>
      </aside>
    </div>

    <section className="results-section"><div className="results-heading"><div><span className="step">04</span><h2>Ready to review</h2></div><span className="result-count">{clips.length} CUT{clips.length === 1 ? '' : 'S'}</span></div>
      {clips.length === 0 ? <div className="empty-results"><div className="empty-frame">▭</div><b>Your next cut will land here.</b><span>Completed renders are saved in your review queue.</span></div> : <div className="clip-grid">{clips.slice(0, 6).map((clip) => {
        const filename = clip.clip_path.split(/[\\/]/).pop();
        return <article className="clip-card" key={filename}><video controls preload="metadata" src={`/clips/${encodeURIComponent(filename)}`} /><div className="clip-meta"><div className="clip-score">RETENTION {Math.round(clip.score * 100)}%</div><h3>{clip.title}</h3><p>{clip.virality_reason || clip.rationale}</p>{clip.hook_quote && <blockquote>“{clip.hook_quote}”</blockquote>}<div className="clip-bottom"><span>{fmt(clip.end - clip.start)} · {clip.source_title}</span><a href={`/clips/${encodeURIComponent(filename)}`} download>Download MP4 ↓</a></div></div></article>;
      })}</div>}
    </section>
    <footer><span>Cutroom</span><span>Built for thoughtful cuts. Runs on this machine.</span></footer>
  </main>;
}

createRoot(document.getElementById('root')).render(<App />);
