@echo off
set STASH_URL=http://localhost:9999
set STASH_API_KEY=eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJ1aWQiOiJldGhvcmsiLCJzdWIiOiJBUElLZXkiLCJpYXQiOjE3Nzc5MjYxOTB9.AjePvgdHp1bfEKt0iAowvxYLipBJyeEpS4sLipAxB8k
REM Paste the value of your reddit_session cookie from a logged-in
REM browser tab (DevTools > Application > Cookies > reddit.com).
REM Either just the reddit_session value or a full multi-cookie string
REM ("reddit_session=...; over18=1; ...") works.
set REDDIT_SESSION_COOKIE=
set REDDIT_USER_AGENT=binge-server/0.1
set BINGE_DB_PATH=binge-server.db
set BINGE_LISTEN_ADDR=0.0.0.0:7878
set BINGE_POLL_INTERVAL=4h
set BINGE_PERFORMER_SYNC_INTERVAL=24h
binge-server.exe
pause
