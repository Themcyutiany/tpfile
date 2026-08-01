@echo off
rem tpfile Windows installer wrapper (double-click or run: install.bat [exe])
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
pause
