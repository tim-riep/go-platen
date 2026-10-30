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
  media type) are set by you when the script asks: once at the start for
  the baseline (600 dpi, toner save off, plain paper), and before and after
  each case that changes one of them. The script reads resolution, toner
  save and media type back from every capture and offers to capture again
  if they do not match.

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
# change in the driver's printing preferences and Restore its baseline
# value. Resolution, Economode and MediaType are the values the PJL header
# of the capture must show; a missing key means the baseline value ('600',
# 'OFF', 'REGULAR') and $null means "do not check".
$cases = @(
    @{ Folder = '03-calibration-600';    Pdf = '01-calibration-a4.pdf';     Paper = 'A4' }
    @{ Folder = '04-calibration-300';    Pdf = '01-calibration-a4.pdf';     Paper = 'A4'; Manual = 'Resolution: 300 dpi'; Restore = 'Resolution: 600 dpi'; Resolution = '300' }
    @{ Folder = '05-calibration-hq1200'; Pdf = '01-calibration-a4.pdf';     Paper = 'A4'; Manual = 'Resolution: HQ 1200'; Restore = 'Resolution: 600 dpi'; Resolution = $null }
    @{ Folder = '06-full-black';         Pdf = '02-full-black-a4.pdf';      Paper = 'A4' }
    @{ Folder = '07-blank';              Pdf = '03-blank-a4.pdf';           Paper = 'A4' }
    @{ Folder = '08-stripes';            Pdf = '04-stripes-a4.pdf';         Paper = 'A4' }
    @{ Folder = '09-grays';              Pdf = '05-grays-a4.pdf';           Paper = 'A4' }
    @{ Folder = '10-grays-toner-save';   Pdf = '05-grays-a4.pdf';           Paper = 'A4'; Manual = 'Toner Save Mode: On'; Restore = 'Toner Save Mode: Off'; Economode = 'ON' }
    @{ Folder = '11-landscape';          Pdf = '06-landscape-a4.pdf';       Paper = 'A4'; Orientation = 'landscape' }
    @{ Folder = '12-letter';             Pdf = '07-calibration-letter.pdf'; Paper = 'Letter' }
    @{ Folder = '13-a5';                 Pdf = '08-calibration-a5.pdf';     Paper = 'A5' }
    @{ Folder = '14-copies-collated';    Pdf = '09-three-pages-a4.pdf';     Paper = 'A4'; Copies = 2; Collate = $true }
    @{ Folder = '15-copies-uncollated';  Pdf = '09-three-pages-a4.pdf';     Paper = 'A4'; Copies = 2; Collate = $false }
    @{ Folder = '16-manual-feed';        Pdf = '01-calibration-a4.pdf';     Paper = 'A4'; Manual = 'Paper Source: Manual'; Restore = 'Paper Source: as in the baseline (Auto Select or Tray 1)' }
    @{ Folder = '17-thick-paper';        Pdf = '01-calibration-a4.pdf';     Paper = 'A4'; Manual = 'Media Type: Thick Paper'; Restore = 'Media Type: Plain Paper'; MediaType = $null }
    # Optional: the same blank page under a file name starting with an
    # umlaut, to explain the byte in front of the PJL job name.
    @{ Folder = '18-job-name';           Pdf = '03-blank-a4.pdf';           Paper = 'A4'; RenameTo = ([string][char]0x00C4) + ' test.pdf'; Optional = $true }
)

# The baseline every case starts from. The driver's own defaults differ
# (e.g. HQ 1200), so the script asks for these settings once at the start.
$baseline = @(
    'Resolution: 600 dpi'
    'Toner Save Mode: Off'
    'Media Type: Plain Paper'
    'Paper Source: Auto Select (or Tray 1)'
    'Paper Size: A4, Orientation: Portrait, Copies: 1'
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
    # The spooler writes the file while it processes the job. Wait until the
    # queue is empty and the file has stopped growing.
    $deadline = (Get-Date).AddMinutes(5)
    $lastSize = -1
    $lastReport = Get-Date
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Seconds 2
        $jobs = @(Get-PrintJob -PrinterName $captureName -ErrorAction SilentlyContinue)
        $size = 0
        if (Test-Path $spoolFile) { $size = (Get-Item $spoolFile).Length }
        if (((Get-Date) - $lastReport).TotalSeconds -ge 10) {
            $states = ($jobs | ForEach-Object { "$($_.JobStatus)" }) -join ', '
            Write-Host ("  waiting: {0} job(s) in the queue {1}, file {2} bytes" -f $jobs.Count, $(if ($states) { "($states)" } else { '' }), $size)
            $lastReport = Get-Date
        }
        if ($jobs.Count -gt 0 -or -not (Test-Path $spoolFile)) { continue }
        $file = Get-Item $spoolFile
        if ($file.LastWriteTime -lt $since) { continue }
        if ($file.Length -gt 0 -and $file.Length -eq $lastSize) { return }
        $lastSize = $file.Length
    }
    throw "The print job did not finish within 5 minutes. Check the queue of '$captureName'."
}

function Open-Preferences([string]$message, [string[]]$settings) {
    Write-Host $message
    foreach ($line in $settings) { Write-Host "    $line" -ForegroundColor Yellow }
    Write-Host 'Leave everything else unchanged and click OK.'
    # The dialog can close or end up behind other windows when the console
    # does not have the focus, so Enter opens it again until the settings
    # are confirmed.
    while ($true) {
        Start-Process rundll32.exe -ArgumentList @('printui.dll,PrintUIEntry', '/e', '/n', "`"$captureName`"") -Wait
        $answer = Read-Host 'Type ok when the settings are saved, or press Enter to open the dialog again'
        if ($answer -match '^\s*(ok|y|yes|j|ja)\s*$') { return }
        foreach ($line in $settings) { Write-Host "    $line" -ForegroundColor Yellow }
    }
}

function Read-PjlSettings([string]$path) {
    # The PJL header is plain text at the start of the stream.
    $bytes = [IO.File]::ReadAllBytes($path)
    $head = [Text.Encoding]::ASCII.GetString($bytes, 0, [Math]::Min($bytes.Length, 4096))
    $values = @{}
    foreach ($m in [regex]::Matches($head, '@PJL SET (\w+)=([^\r\n]*)')) {
        $values[$m.Groups[1].Value] = $m.Groups[2].Value.Trim()
    }
    $values
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
Write-Host ''
Open-Preferences "The printing preferences of '$captureName' open now. Set the baseline for all captures:" $baseline

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

    $collate = $false
    if ($case.ContainsKey('Collate')) { $collate = [bool]$case.Collate }
    Set-PrintConfiguration -PrinterName $captureName -Collate $collate
    $manual = ''
    if ($case.ContainsKey('Manual')) {
        $manual = $case.Manual
        Open-Preferences "The printing preferences of '$captureName' open now. Set:" @($manual)
    }

    # Expected values in the PJL header.
    $expect = @{ RESOLUTION = '600'; ECONOMODE = 'OFF'; MEDIATYPE = 'REGULAR' }
    if ($case.ContainsKey('Resolution')) { $expect.RESOLUTION = $case.Resolution }
    if ($case.ContainsKey('Economode')) { $expect.ECONOMODE = $case.Economode }
    if ($case.ContainsKey('MediaType')) { $expect.MEDIATYPE = $case.MediaType }

    # SumatraPDF's paper names: A4, A5, letter.
    $sumatraPaper = $case.Paper
    if ($sumatraPaper -eq 'Letter') { $sumatraPaper = 'letter' }
    $settings = @('noscale', "paper=$sumatraPaper")
    if ($case.ContainsKey('Orientation')) { $settings += $case.Orientation }
    $copies = 1
    if ($case.ContainsKey('Copies')) { $copies = [int]$case.Copies; $settings += "$($copies)x" }
    $printSettings = $settings -join ','

    $pdf = Join-Path $here $case.Pdf
    while ($true) {
        $printPdf = $pdf
        $tempDir = $null
        if ($case.ContainsKey('RenameTo')) {
            $tempDir = Join-Path ([IO.Path]::GetTempPath()) ('platen-' + [guid]::NewGuid())
            New-Item -ItemType Directory -Path $tempDir | Out-Null
            $printPdf = Join-Path $tempDir $case.RenameTo
            Copy-Item $pdf $printPdf
        }
        if (Test-Path $spoolFile) { Remove-Item $spoolFile -Force }
        Write-Host "Printing $($case.Pdf) ($printSettings)..."
        $start = Get-Date
        $sumatraArgs = @('-print-to', "`"$captureName`"", '-print-settings', "`"$printSettings`"", '-silent', '-exit-when-done', "`"$printPdf`"")
        $proc = Start-Process -FilePath $sumatra -ArgumentList $sumatraArgs -PassThru
        if (-not $proc.WaitForExit(180000)) {
            throw 'SumatraPDF did not finish within 3 minutes. It may be showing a message; close it and run the script again.'
        }
        Wait-ForCapture $start
        if ($tempDir) { Remove-Item -Recurse -Force $tempDir }

        $pjl = Read-PjlSettings $spoolFile
        $mismatch = @()
        foreach ($key in @('RESOLUTION', 'ECONOMODE', 'MEDIATYPE')) {
            $want = $expect[$key]
            $got = $pjl[$key]
            if ($null -ne $want -and $got -ne $want) { $mismatch += "$key=$got (expected $want)" }
        }
        if ($mismatch.Count -eq 0) { break }
        Write-Host "The capture does not show the expected settings: $($mismatch -join '; ')" -ForegroundColor Red
        $answer = Read-Host 'Correct the settings and capture again? (Y/n)'
        if ($answer -match '^[nN]') { break }
        Open-Preferences "Correct the settings of '$captureName'. This case needs:" ($baseline + @($manual) | Where-Object { $_ })
    }

    New-Item -ItemType Directory -Force -Path $folder | Out-Null
    Move-Item -Force $spoolFile $target
    Copy-Item -Force $pdf (Join-Path $folder 'source.pdf')

    $pjlLine = (@('RESOLUTION', 'ECONOMODE', 'MEDIATYPE') | ForEach-Object { "$_=$($pjl[$_])" }) -join ', '
    $lines = @(
        "Captured with testdata/capture-requests/capture.ps1 on $(Get-Date -Format 'yyyy-MM-dd HH:mm')."
        "Printer driver: $driverName, version $driverVersion."
        "Operating system: $($os.Caption), version $($os.Version)."
        "Printed with SumatraPDF $sumatraVersion, print settings: $printSettings (noscale = actual size)."
        "Source: $($case.Pdf)$(if ($case.ContainsKey('RenameTo')) { ', printed as file name ' + $case.RenameTo })."
        "Paper size: $($case.Paper)."
        "Orientation: $(if ($case.ContainsKey('Orientation')) { $case.Orientation } else { 'portrait' })."
        "Copies: $copies$(if ($case.ContainsKey('Collate')) { ', collate ' + $(if ($collate) { 'on' } else { 'off' }) })."
        "Baseline in the printing preferences: $($baseline -join '; ')."
        "Driver setting changed for this case: $(if ($manual) { $manual } else { 'none (baseline)' })."
        "Read back from the PJL header of the capture: $pjlLine."
    )
    if ($mismatch.Count -gt 0) { $lines += "WARNING: the capture does not show the expected settings: $($mismatch -join '; ')." }
    # Plain ASCII except for the file name of the optional job name case.
    [IO.File]::WriteAllLines((Join-Path $folder 'settings.txt'), $lines, (New-Object Text.UTF8Encoding $false))
    $size = (Get-Item $target).Length
    Write-Host "Saved $target ($size bytes, $pjlLine)." -ForegroundColor Green
    $done += $case.Folder

    if ($manual) {
        Open-Preferences "The printing preferences open again. Set back to the baseline:" @($case.Restore)
    }
}

Write-Host ''
Write-Host "Captured $($done.Count) case(s): $($done -join ', ')"
Write-Host "The captures are in $capturesRoot."
$remove = Read-Host "Remove the printer '$captureName' and its file port again? (y/N)"
if ($remove -match '^[yYjJ]') {
    Remove-Printer -Name $captureName
    Remove-PrinterPort -Name $spoolFile
    Write-Host 'Removed.'
}
