; Installer for the Platen Windows client. Built by build.sh with
;   makensis -DVERSION=1.2.3 -DVERSION4=1.2.3.0 -DBUILD=<dir> [-DSIGN] platen.nsi
Unicode true
!include MUI2.nsh
!include x64.nsh

Name "Platen"
OutFile "${BUILD}/Platen-${VERSION}-setup.exe"
InstallDir "$PROGRAMFILES64\Platen"
RequestExecutionLevel admin
SetCompressor /SOLID lzma
BrandingText "Platen ${VERSION}"

VIProductVersion "${VERSION4}"
VIFileVersion "${VERSION4}"
VIAddVersionKey "ProductName" "Platen"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "FileDescription" "Platen Setup"
VIAddVersionKey "LegalCopyright" "Licensed under the Apache License, Version 2.0"

!ifdef SIGN
  ; Signed after makensis wrote them; %1 is the file.
  !uninstfinalize '"${SIGNER}" "%1"' = 0
  !finalize '"${SIGNER}" "%1"' = 0
!endif

!define UNINSTKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\Platen"
!define RUNKEY "Software\Microsoft\Windows\CurrentVersion\Run"

!define MUI_ICON "../../cmd/platen-desktop/platen.ico"
!define MUI_UNICON "../../cmd/platen-desktop/platen.ico"
!define MUI_FINISHPAGE_RUN
!define MUI_FINISHPAGE_RUN_TEXT "Start Platen"
!define MUI_FINISHPAGE_RUN_FUNCTION StartPlaten
!insertmacro MUI_PAGE_LICENSE "../../LICENSE"
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ${IfNot} ${RunningX64}
    MessageBox MB_ICONSTOP "Platen needs 64-bit Windows 10 or later."
    Abort
  ${EndIf}
  SetRegView 64
FunctionEnd

Function un.onInit
  SetRegView 64
FunctionEnd

; Starts Platen as the signed-in user, not elevated like the installer.
Function StartPlaten
  Exec '"$WINDIR\explorer.exe" "$INSTDIR\Platen.exe"'
FunctionEnd

Section "Platen"
  ; Running copies would keep Platen.exe locked.
  nsExec::Exec 'taskkill.exe /IM Platen.exe /F'
  Sleep 500
  SetOutPath "$INSTDIR"
  File "${BUILD}/Platen.exe"
  File /oname=LICENSE.txt "../../LICENSE"
  File /oname=THIRD_PARTY_LICENSES.txt "../../THIRD_PARTY_LICENSES"
  WriteUninstaller "$INSTDIR\Uninstall.exe"
  CreateShortcut "$SMPROGRAMS\Platen.lnk" "$INSTDIR\Platen.exe"

  ; The printers use the "Microsoft Print To PDF" driver, an optional
  ; Windows feature that can be turned off.
  nsExec::ExecToLog 'powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -Command "if (-not (Get-PrinterDriver -Name ''Microsoft Print To PDF'' -ErrorAction SilentlyContinue)) { Enable-WindowsOptionalFeature -Online -FeatureName Printing-PrintToPDFServices-Features -All -NoRestart | Out-Null }"'

  WriteRegStr HKLM "${UNINSTKEY}" "DisplayName" "Platen"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKLM "${UNINSTKEY}" "Publisher" "platen"
  WriteRegStr HKLM "${UNINSTKEY}" "URLInfoAbout" "https://github.com/tim-riep/go-platen"
  WriteRegStr HKLM "${UNINSTKEY}" "DisplayIcon" "$INSTDIR\Platen.exe"
  WriteRegStr HKLM "${UNINSTKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKLM "${UNINSTKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr HKLM "${UNINSTKEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoModify" 1
  WriteRegDWORD HKLM "${UNINSTKEY}" "NoRepair" 1
SectionEnd

Section "Uninstall"
  nsExec::Exec 'taskkill.exe /IM Platen.exe /F'
  Sleep 500
  ; The printers belong to the computer, so they go for all users.
  nsExec::ExecToLog '"$INSTDIR\Platen.exe" remove-printers'
  DeleteRegValue HKCU "${RUNKEY}" "Platen"
  Delete "$SMPROGRAMS\Platen.lnk"
  Delete "$INSTDIR\Platen.exe"
  Delete "$INSTDIR\LICENSE.txt"
  Delete "$INSTDIR\THIRD_PARTY_LICENSES.txt"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  DeleteRegKey HKLM "${UNINSTKEY}"
SectionEnd
