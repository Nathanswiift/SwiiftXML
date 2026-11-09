@echo off
rem Builds SwiiftXML.exe on Windows. Requires Go 1.27 or newer (https://go.dev/dl/).
cd /d "%~dp0"
set CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o SwiiftXML.exe .
if errorlevel 1 exit /b 1
echo Built SwiiftXML.exe
