# forge-hris landing page

Static marketing site for [forge-hris](https://github.com/azmi2409/forge-hris) —
the open-source, agent-ready HRIS. Pure HTML + CSS, **no framework, no build
step, no JavaScript**. Works with JS disabled. Deployable to any static host.

Files:

- `index.html` — page content
- `styles.css` — all styling

## Preview locally

```bash
cd landing
python3 -m http.server 8000
# then open http://localhost:8000
```

or with Node:

```bash
cd landing
npx serve .
```

## Deploy

Any static host works. No build step required — serve the directory as-is.

- **GitHub Pages:** push `landing/` as the repo's `docs/` folder (or use the
  `gh-pages` branch / Pages "Deploy from a branch" pointing at the folder).
- **Cloudflare Pages:** "Direct upload" the `landing/` directory, or connect
  the repo with the output directory set to `landing/`.
- **Any static host / nginx:** copy `landing/` to the web root.
- **S3 + CloudFront:** `aws s3 sync landing/ s3://your-bucket/ --exclude README.md`
  and serve `index.html` as the default root object.

## Content rules

Content claims on this page must stay in sync with the repo
[`README.md`](../README.md) and `docs/`. Do **not** add features, metrics,
testimonials, or pricing. Specifically:

- Statutory coverage is Malaysia (EPF/SOCSO/EIS/PCB/HRDF) and Indonesia
  (BPJS Kesehatan, BPJS Ketenagakerjaan, PPh 21) only.
- Rule packs are versioned and must be re-verified against official
  LHDN/EPF/SOCSO/BPJS sources before any real payroll run — this warning
  is on the page ("Good to know") and must not be removed.
- Deliberately out of scope: face recognition, GPS tracking, screenshots,
  half-day leave, native mobile apps, Singapore payroll.
