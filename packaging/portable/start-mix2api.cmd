@echo off
rem Portable Windows launcher. Credentials come from the environment or a
rem local .env-style wrapper; this file never stores secrets.
setlocal
cd /d "%~dp0"
if "%ACCOUNTS_DIR%"=="" set "ACCOUNTS_DIR=%CD%\accounts"
if "%USAGE_DIR%"=="" set "USAGE_DIR=%CD%\usage"
if "%WB_MEDIA_DIR%"=="" set "WB_MEDIA_DIR=%CD%\media"
if not exist "%ACCOUNTS_DIR%" mkdir "%ACCOUNTS_DIR%"
if not exist "%USAGE_DIR%" mkdir "%USAGE_DIR%"
if not exist "%WB_MEDIA_DIR%" mkdir "%WB_MEDIA_DIR%"
if "%HOST%"=="" set "HOST=127.0.0.1"
if "%PORT%"=="" set "PORT=58675"
echo Starting mix2api on %HOST%:%PORT%
start "mix2api" /min "%~dp0mix2api.exe"
endlocal
