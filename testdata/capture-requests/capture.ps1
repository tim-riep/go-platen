#Requires -Version 5.1
<#
.SYNOPSIS
  Captures the print data streams listed in README.md with the official
  Brother driver.

.DESCRIPTION
  Run from an elevated PowerShell (Run as administrator) in this folder:

    powershell -ExecutionPolicy Bypass -File .\capture.ps1

  The script asks which installed printer's driver to use. It creates a
  printer "platen capture" with that driver on a local port that writes to a
  file, prints every test page with the settings from README.md and stores
  the result as ..\captures\<folder>\capture.prn together with source.pdf
  and settings.txt. Nothing is sent to a real printer.

  The PDFs are printed with SumatraPDF at actual size. Settings that only
  the driver's own dialog offers (resolution, toner save, paper source,
  media type) are set by you when the script asks for it; the script resets
  the printer to its defaults before every capture.

.PARAMETER SumatraPath
  Path to SumatraPDF.exe, if it is not found automatically.

.PARAMETER Only
  Capture only these folders, e.g. -Only 03-calibration-600,08-stripes.

.PARAMETER Force
  Capture again even if the folder already contains a capture.
#>
[CmdletBinding()]
param(
    [string]$SumatraPath,
    [string[]]$Only,
    [switch]$Force
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$here = $PSScriptRoot
$capturesRoot = Join-Path (Split-Path $here -Parent) 'captures'
$captureName = 'platen capture'
$spoolFile = Join-Path $capturesRoot '_spool.prn'

# Cases from README.md. Paper, Orientation and Copies are passed to
# SumatraPDF; Collate is set on the printer; Manual names a setting you
# change in the driver's printing preferences.
$cases = @(
    @{ Folder = '03-calibration-600';    Pdf = '01-calibration-a4.pdf';     Paper = 'A4' }
    @{ Folder = '04-calibration-300';    Pdf = '01-calibration-a4.pdf';     Paper = 'A4'; Manual = 'Resolution: 300 dpi' }
    @{ Folder = '05-calibration-hq1200'; Pdf = '01-calibration-a4.pdf';     Paper = 'A4'; Manual = 'Resolution: HQ 1200' }
    @{ Folder = '06-full-black';         Pdf = '02-full-black-a4.pdf';      Paper = 'A4' }
    @{ Folder = '07-blank';              Pdf = '03-blank-a4.pdf';           Paper = 'A4' }
    @{ Folder = '08-stripes';            Pdf = '04-stripes-a4.pdf';         Paper = 'A4' }
    @{ Folder = '09-grays';              Pdf = '05-grays-a4.pdf';           Paper = 'A4' }
    @{ Folder = '10-grays-toner-save';   Pdf = '05-grays-a4.pdf';           Paper = 'A4'; Manual = 'Toner Save Mode: On' }
    @{ Folder = '11-landscape';          Pdf = '06-landscape-a4.pdf';       Paper = 'A4'; Orientation = 'landscape' }
    @{ Folder = '12-letter';             Pdf = '07-calibration-letter.pdf'; Paper = 'Letter' }
    @{ Folder = '13-a5';                 Pdf = '08-calibration-a5.pdf';     Paper = 'A5' }
    @{ Folder = '14-copies-collated';    Pdf = '09-three-pages-a4.pdf';     Paper = 'A4'; Copies = 2; Collate = $true }
    @{ Folder = '15-copies-uncollated';  Pdf = '09-three-pages-a4.pdf';     Paper = 'A4'; Copies = 2; Collate = $false }
    @{ Folder = '16-manual-feed';        Pdf = '01-calibration-a4.pdf';     Paper = 'A4'; Manual = 'Paper Source: Manual' }
    @{ Folder = '17-thick-paper';        Pdf = '01-calibration-a4.pdf';     Paper = 'A4'; Manual = 'Media Type: Thick Paper' }
    # Optional: the same blank page under a file name starting with an
    # umlaut, to explain the byte in front of the PJL job name.
    @{ Folder = '18-job-name';           Pdf = '03-blank-a4.pdf';           Paper = 'A4'; RenameTo = ([string][char]0x00C4) + ' test.pdf'; Optional = $true }
)

function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal $id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Format-DriverVersion([UInt64]$v) {
    '{0}.{1}.{2}.{3}' -f (($v -shr 48) -band 0xffff), (($v -shr 32) -band 0xffff), (($v -shr 16) -band 0xffff), ($v -band 0xffff)
}

function Find-Sumatra {
    if ($SumatraPath) {
        if (Test-Path $SumatraPath) { return (Resolve-Path $SumatraPath).Path }
        throw "SumatraPDF not found at $SumatraPath"
    }
    $candidates = @(
        (Join-Path $env:LOCALAPPDATA 'SumatraPDF\SumatraPDF.exe'),
        (Join-Path $env:ProgramFiles 'SumatraPDF\SumatraPDF.exe')
    )
    if (${env:ProgramFiles(x86)}) { $candidates += (Join-Path ${env:ProgramFiles(x86)} 'SumatraPDF\SumatraPDF.exe') }
    foreach ($c in $candidates) { if (Test-Path $c) { return $c } }
    $cmd = Get-Command SumatraPDF.exe -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    return $null
}

function Wait-ForCapture([datetime]$since) {
    # The spooler writes the file when the job is processed. Wait until the
    # queue is empty and the file has stopped growing.
    $deadline = (Get-Date).AddMinutes(5)
    $lastSize = -1
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 2
        $jobs = @(Get-PrintJob -PrinterName $captureName -ErrorAction SilentlyContinue)
        if ($jobs.Count -gt 0 -or -not (Test-Path $spoolFile)) { continue }
        $file = Get-Item $spoolFile
        if ($file.LastWriteTime -lt $since) { continue }
        if ($file.Length -gt 0 -and $file.Length -eq $lastSize) { return }
        $lastSize = $file.Length
    }
    throw "The print job did not finish within 5 minutes. Check the queue of '$captureName'."
}

if (-not (Test-Admin)) {
    Write-Host "Please run this script from an elevated PowerShell (Run as administrator)." -ForegroundColor Yellow
    Write-Host "It needs to create a printer and a printer port that writes to a file."
    exit 1
}

# --- choose the printer whose driver produces the captures ---
$printers = @(Get-Printer | Where-Object { $_.Name -ne $captureName } | Sort-Object Name)
if ($printers.Count -eq 0) { throw 'No printers are installed.' }
Write-Host ''
Write-Host 'Installed printers:'
for ($i = 0; $i -lt $printers.Count; $i++) {
    Write-Host ('  [{0}] {1}   (driver: {2})' -f $i, $printers[$i].Name, $printers[$i].DriverName)
}
$choice = Read-Host 'Number of the Brother HL-2135W printer whose driver should be used'
$index = 0
if (-not [int]::TryParse($choice, [ref]$index) -or $index -lt 0 -or $index -ge $printers.Count) {
    throw "Invalid choice: $choice"
}
$driverName = $printers[$index].DriverName
$driver = Get-PrinterDriver -Name $driverName
$driverVersion = Format-DriverVersion $driver.DriverVersion
Write-Host "Using driver '$driverName', version $driverVersion."

$sumatra = Find-Sumatra
if (-not $sumatra) {
    Write-Host ''
    Write-Host 'SumatraPDF was not found. It prints PDFs at actual size from the command line.' -ForegroundColor Yellow
    Write-Host 'Install it from https://www.sumatrapdfreader.org or pass -SumatraPath.'
    exit 1
}
$sumatraVersion = (Get-Item $sumatra).VersionInfo.ProductVersion

# --- capture printer on a file port ---
New-Item -ItemType Directory -Force -Path $capturesRoot | Out-Null
if (-not (Get-PrinterPort -Name $spoolFile -ErrorAction SilentlyContinue)) {
    Add-PrinterPort -Name $spoolFile
}
$capturePrinter = Get-Printer -Name $captureName -ErrorAction SilentlyContinue
if ($capturePrinter -and $capturePrinter.DriverName -ne $driverName) {
    Remove-Printer -Name $captureName
    $capturePrinter = $null
}
if (-not $capturePrinter) {
    Add-Printer -Name $captureName -DriverName $driverName -PortName $spoolFile
} elseif ($capturePrinter.PortName -ne $spoolFile) {
    Set-Printer -Name $captureName -PortName $spoolFile
}
$defaultTicket = (Get-PrintConfiguration -PrinterName $captureName).PrintTicketXML

$os = Get-CimInstance Win32_OperatingSystem
$selected = $cases
if ($Only) { $selected = @($cases | Where-Object { $Only -contains $_.Folder }) }

$done = @()
foreach ($case in $selected) {
    $folder = Join-Path $capturesRoot $case.Folder
    $target = Join-Path $folder 'capture.prn'
    if ((Test-Path $target) -and -not $Force) {
        Write-Host "Skipping $($case.Folder): capture exists (use -Force to capture again)."
        continue
    }
    if ($case.ContainsKey('Optional') -and $case.Optional -and -not $Only) {
        $answer = Read-Host "Capture the optional case $($case.Folder)? (y/N)"
        if ($answer -notmatch '^[yYjJ]') { continue }
    }
    Write-Host ''
    Write-Host "=== $($case.Folder) ===" -ForegroundColor Cyan

    # Start from the driver defaults.
    Set-PrintConfiguration -PrinterName $captureName -PrintTicketXml $defaultTicket
    $collate = $false
    if ($case.ContainsKey('Collate')) {
        $collate = [bool]$case.Collate
        Set-PrintConfiguration -PrinterName $captureName -Collate $collate
    }
    $manual = ''
    if ($case.ContainsKey('Manual')) {
        $manual = $case.Manual
        Write-Host "The printing preferences of '$captureName' open now."
        Write-Host "Set:  $manual" -ForegroundColor Yellow
        Write-Host 'Leave everything else unchanged and click OK.'
        Start-Process rundll32.exe -ArgumentList @('printui.dll,PrintUIEntry', '/e', '/n', "`"$captureName`"") -Wait
        Read-Host 'Press Enter when the setting is saved'
    }

    # Print with SumatraPDF at actual size.
    $pdf = Join-Path $here $case.Pdf
    $printPdf = $pdf
    $tempDir = $null
    if ($case.ContainsKey('RenameTo')) {
        $tempDir = Join-Path ([IO.Path]::GetTempPath()) ('platen-' + [guid]::NewGuid())
        New-Item -ItemType Directory -Path $tempDir | Out-Null
        $printPdf = Join-Path $tempDir $case.RenameTo
        Copy-Item $pdf $printPdf
    }
    # SumatraPDF's paper names: A4, A5, letter.
    $sumatraPaper = $case.Paper
    if ($sumatraPaper -eq 'Letter') { $sumatraPaper = 'letter' }
    $settings = @('noscale', "paper=$sumatraPaper")
    if ($case.ContainsKey('Orientation')) { $settings += $case.Orientation }
    $copies = 1
    if ($case.ContainsKey('Copies')) { $copies = [int]$case.Copies; $settings += "$($copies)x" }
    $printSettings = $settings -join ','

    if (Test-Path $spoolFile) { Remove-Item $spoolFile -Force }
    $start = Get-Date
    $sumatraArgs = @('-print-to', "`"$captureName`"", '-print-settings', "`"$printSettings`"", '-silent', '-exit-when-done', "`"$printPdf`"")
    Start-Process -FilePath $sumatra -ArgumentList $sumatraArgs -Wait
    Wait-ForCapture $start

    New-Item -ItemType Directory -Force -Path $folder | Out-Null
    Move-Item -Force $spoolFile $target
    Copy-Item -Force $pdf (Join-Path $folder 'source.pdf')
    if ($tempDir) { Remove-Item -Recurse -Force $tempDir }

    $lines = @(
        "Captured with testdata/capture-requests/capture.ps1 on $(Get-Date -Format 'yyyy-MM-dd HH:mm')."
        "Printer driver: $driverName, version $driverVersion."
        "Operating system: $($os.Caption), version $($os.Version)."
        "Printed with SumatraPDF $sumatraVersion, print settings: $printSettings (noscale = actual size)."
        "Source: $($case.Pdf)$(if ($case.ContainsKey('RenameTo')) { ', printed as file name ' + $case.RenameTo })."
        "Paper size: $($case.Paper)."
        "Orientation: $(if ($case.ContainsKey('Orientation')) { $case.Orientation } else { 'portrait' })."
        "Copies: $copies$(if ($case.ContainsKey('Collate')) { ', collate ' + $(if ($collate) { 'on' } else { 'off' }) })."
        "Driver setting changed in the printing preferences: $(if ($manual) { $manual } else { 'none (driver defaults)' })."
    )
    # Plain ASCII except for the file name of the optional job name case.
    [IO.File]::WriteAllLines((Join-Path $folder 'settings.txt'), $lines, (New-Object Text.UTF8Encoding $false))
    $size = (Get-Item $target).Length
    Write-Host "Saved $target ($size bytes)." -ForegroundColor Green
    $done += $case.Folder

    if ($manual) {
        # The printing preferences are stored per user; set the value back
        # so that the next capture starts from the driver defaults.
        Write-Host "The printing preferences open again. Set back:  $manual  (to the default value)" -ForegroundColor Yellow
        Start-Process rundll32.exe -ArgumentList @('printui.dll,PrintUIEntry', '/e', '/n', "`"$captureName`"") -Wait
        Read-Host 'Press Enter when the setting is back at its default'
    }
}

# Back to the driver defaults.
Set-PrintConfiguration -PrinterName $captureName -PrintTicketXml $defaultTicket

Write-Host ''
Write-Host "Captured $($done.Count) case(s): $($done -join ', ')"
Write-Host "The captures are in $capturesRoot."
$remove = Read-Host "Remove the printer '$captureName' and its file port again? (y/N)"
if ($remove -match '^[yYjJ]') {
    Remove-Printer -Name $captureName
    Remove-PrinterPort -Name $spoolFile
    Write-Host 'Removed.'
}
