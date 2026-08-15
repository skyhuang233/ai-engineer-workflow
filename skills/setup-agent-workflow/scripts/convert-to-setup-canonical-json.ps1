[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$InputPath,
    [Parameter(Mandatory = $true)][string]$OutputPath
)

$ErrorActionPreference = "Stop"
if (-not ("SetupUnicodeCodePointComparer" -as [type])) {
Add-Type -TypeDefinition @'
using System;
using System.Collections;
using System.Collections.Generic;
public sealed class SetupUnicodeCodePointComparer : IComparer<string> {
    public static readonly SetupUnicodeCodePointComparer Instance = new SetupUnicodeCodePointComparer();
    public static string[] SortedKeys(IDictionary values) {
        string[] keys = new string[values.Keys.Count]; int i = 0;
        foreach (object key in values.Keys) keys[i++] = Convert.ToString(key);
        Array.Sort(keys, Instance); return keys;
    }
    public int Compare(string left, string right) {
        int li = 0, ri = 0;
        while (li < left.Length && ri < right.Length) {
            int lp = Char.ConvertToUtf32(left, li), rp = Char.ConvertToUtf32(right, ri);
            if (lp != rp) return lp < rp ? -1 : 1;
            li += Char.IsSurrogatePair(left, li) ? 2 : 1;
            ri += Char.IsSurrogatePair(right, ri) ? 2 : 1;
        }
        return (left.Length - li).CompareTo(right.Length - ri);
    }
}
'@
}
function Write-SetupJsonString([Text.StringBuilder]$Builder, [string]$Value) {
    [void]$Builder.Append('"')
    foreach ($character in $Value.ToCharArray()) {
        $number = [int]$character; $escaped = $true
        switch ($number) {
            8 { [void]$Builder.Append('\b') }; 9 { [void]$Builder.Append('\t') }; 10 { [void]$Builder.Append('\n') }
            12 { [void]$Builder.Append('\f') }; 13 { [void]$Builder.Append('\r') }; 34 { [void]$Builder.Append('\"') }; 92 { [void]$Builder.Append('\\') }
            default { $escaped = $false }
        }
        if ($escaped) { continue }
        if ($number -lt 32) { [void]$Builder.Append(('\u{0:x4}' -f $number)) } else { [void]$Builder.Append($character) }
    }
    [void]$Builder.Append('"')
}
function Write-SetupCanonicalValue([Text.StringBuilder]$Builder, $Value) {
    if ($null -eq $Value) { [void]$Builder.Append('null'); return }
    if ($Value -is [string]) { Write-SetupJsonString $Builder $Value; return }
    if ($Value -is [bool]) { [void]$Builder.Append($(if ($Value) { 'true' } else { 'false' })); return }
    if ($Value -is [byte] -or $Value -is [sbyte] -or $Value -is [int16] -or $Value -is [uint16] -or $Value -is [int32] -or $Value -is [uint32] -or $Value -is [int64] -or $Value -is [uint64]) {
        [void]$Builder.Append($Value.ToString([Globalization.CultureInfo]::InvariantCulture)); return
    }
    if ($Value -is [Collections.IDictionary]) {
        [string[]]$keys = [SetupUnicodeCodePointComparer]::SortedKeys($Value); [void]$Builder.Append('{')
        for ($index = 0; $index -lt $keys.Count; $index++) {
            if ($index -gt 0) { [void]$Builder.Append(',') }
            Write-SetupJsonString $Builder $keys[$index]; [void]$Builder.Append(':'); Write-SetupCanonicalValue $Builder $Value[$keys[$index]]
        }
        [void]$Builder.Append('}'); return
    }
    if ($Value -is [Management.Automation.PSCustomObject]) {
        [string[]]$keys = @($Value.PSObject.Properties.Name)
        [Array]::Sort($keys, [SetupUnicodeCodePointComparer]::Instance)
        [void]$Builder.Append('{')
        for ($index = 0; $index -lt $keys.Count; $index++) {
            if ($index -gt 0) { [void]$Builder.Append(',') }
            Write-SetupJsonString $Builder $keys[$index]; [void]$Builder.Append(':'); Write-SetupCanonicalValue $Builder $Value.PSObject.Properties[$keys[$index]].Value
        }
        [void]$Builder.Append('}'); return
    }
    if ($Value -is [Collections.IEnumerable]) {
        [void]$Builder.Append('['); $index = 0
        foreach ($item in $Value) { if ($index++ -gt 0) { [void]$Builder.Append(',') }; Write-SetupCanonicalValue $Builder $item }
        [void]$Builder.Append(']'); return
    }
    throw "Floating-point or unsupported JSON value type $($Value.GetType().FullName)"
}

$utf8 = New-Object Text.UTF8Encoding($false, $true)
$raw = [IO.File]::ReadAllText([IO.Path]::GetFullPath($InputPath), $utf8)
$value = $raw | ConvertFrom-Json
$builder = New-Object Text.StringBuilder
Write-SetupCanonicalValue $builder $value
$canonical = $builder.ToString()
[IO.File]::WriteAllText([IO.Path]::GetFullPath($OutputPath), $canonical, $utf8)
$hasher = [Security.Cryptography.SHA256]::Create()
try { ([BitConverter]::ToString($hasher.ComputeHash($utf8.GetBytes($canonical)))).Replace('-', '').ToLowerInvariant() } finally { $hasher.Dispose() }
