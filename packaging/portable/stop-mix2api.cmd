@echo off
rem Stop the mix2api.exe started from this folder. Uses the image name; the
rem binary is unique to this portable directory in normal use.
setlocal
taskkill /IM mix2api.exe /F >nul 2>&1
if errorlevel 1 (
  echo mix2api.exe is not running
  exit /b 1
)
echo mix2api stopped
endlocal
