; ตัวติดตั้ง PhoneDrop สำหรับ Windows — ติดตั้งเฉพาะผู้ใช้ปัจจุบัน ไม่ต้องใช้สิทธิ์ผู้ดูแลระบบ
; build: makensis installer.nsi   (ต้อง build dist\PhoneDrop.exe ก่อน)

Unicode true
!define APP "PhoneDrop"
!define VERSION "2.1.0"
!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\PhoneDrop"
!define RUN_KEY "Software\Microsoft\Windows\CurrentVersion\Run"

Name "${APP}"
OutFile "dist\PhoneDrop-Setup.exe"
InstallDir "$LOCALAPPDATA\Programs\PhoneDrop"
; ถ้าเคยติดตั้งแล้ว ให้เสนอโฟลเดอร์เดิมเป็นค่าเริ่มต้น
InstallDirRegKey HKCU "${UNINST_KEY}" "InstallLocation"
RequestExecutionLevel user
SetCompressor /SOLID lzma
Icon "assets\icon.ico"
UninstallIcon "assets\icon.ico"
AutoCloseWindow true
ShowInstDetails nevershow
ShowUninstDetails nevershow

; ให้ผู้ใช้เลือกโฟลเดอร์ติดตั้ง (ถ้ากด Browse เลือกโฟลเดอร์อื่น ตัวติดตั้งจะต่อท้ายด้วย \PhoneDrop ให้เอง)
Page directory "" "" DirLeave
Page instfiles
UninstPage uninstConfirm
UninstPage instfiles

LoadLanguageFile "${NSISDIR}\Contrib\Language files\Thai.nlf"

VIProductVersion "${VERSION}.0"
VIAddVersionKey "ProductName" "${APP}"
VIAddVersionKey "FileDescription" "${APP} Setup"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "LegalCopyright" ""

Var OldDir

; ตรวจว่าเขียนลงโฟลเดอร์ที่เลือกได้ (เช่น Program Files ต้องใช้สิทธิ์ผู้ดูแลระบบ ซึ่งตัวติดตั้งนี้ไม่ขอ)
Function DirLeave
  ClearErrors
  CreateDirectory "$INSTDIR"
  FileOpen $0 "$INSTDIR\.phonedrop-write-test" w
  IfErrors 0 writable
    RMDir "$INSTDIR"
    MessageBox MB_ICONEXCLAMATION "ไม่มีสิทธิ์เขียนลงโฟลเดอร์นี้ กรุณาเลือกโฟลเดอร์อื่น"
    Abort
  writable:
  FileClose $0
  Delete "$INSTDIR\.phonedrop-write-test"
FunctionEnd

Section "Install"
  ReadRegStr $OldDir HKCU "${UNINST_KEY}" "InstallLocation"

  ; ปิดตัวที่เปิดอยู่ก่อน จะได้เขียนทับไฟล์ได้
  nsExec::Exec 'taskkill /F /IM PhoneDrop.exe'
  Pop $0
  Sleep 700

  ; ย้ายที่ติดตั้ง: ลบไฟล์โปรแกรมจากโฟลเดอร์เดิม (ลบเฉพาะไฟล์ของเรา ไม่ลบทั้งโฟลเดอร์)
  StrCmp $OldDir "" noOld
  StrCmp $OldDir "$INSTDIR" noOld
    Delete "$OldDir\PhoneDrop.exe"
    Delete "$OldDir\Uninstall.exe"
    RMDir "$OldDir"
  noOld:

  SetOutPath "$INSTDIR"
  File "dist\PhoneDrop.exe"
  WriteUninstaller "$INSTDIR\Uninstall.exe"

  CreateShortcut "$SMPROGRAMS\PhoneDrop.lnk" "$INSTDIR\PhoneDrop.exe"
  CreateShortcut "$DESKTOP\PhoneDrop.lnk" "$INSTDIR\PhoneDrop.exe"

  ; เปิดพร้อมเครื่องแบบซ่อน (ปิดได้จากเมนูที่ไอคอนถาดระบบ)
  WriteRegStr HKCU "${RUN_KEY}" "PhoneDrop" '"$INSTDIR\PhoneDrop.exe" --hidden'

  WriteRegStr HKCU "${UNINST_KEY}" "DisplayName" "${APP}"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\PhoneDrop.exe"
  WriteRegStr HKCU "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNINST_KEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegDWORD HKCU "${UNINST_KEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNINST_KEY}" "NoRepair" 1
SectionEnd

Function .onInstSuccess
  IfSilent +2
  Exec '"$INSTDIR\PhoneDrop.exe"'
FunctionEnd

Section "Uninstall"
  nsExec::Exec 'taskkill /F /IM PhoneDrop.exe'
  Pop $0
  Sleep 700

  Delete "$INSTDIR\PhoneDrop.exe"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR"
  Delete "$SMPROGRAMS\PhoneDrop.lnk"
  Delete "$DESKTOP\PhoneDrop.lnk"
  DeleteRegValue HKCU "${RUN_KEY}" "PhoneDrop"
  DeleteRegKey HKCU "${UNINST_KEY}"
  ; ไฟล์ที่รับมาและการตั้งค่าใน %APPDATA%\PhoneDrop ไม่ถูกลบ
SectionEnd
