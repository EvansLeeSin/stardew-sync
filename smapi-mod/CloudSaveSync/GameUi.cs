using System.Linq.Expressions;
using System.Reflection;

namespace CloudSaveSync;

/// <summary>
/// Minimal access to game UI via reflection, so the mod compiles against
/// StardewModdingAPI.dll only (no game assemblies needed in CI).
/// </summary>
internal static class GameUi
{
    private const BindingFlags Static = BindingFlags.Public | BindingFlags.NonPublic | BindingFlags.Static;
    private static readonly Dictionary<string, Type?> Types = new();

    private static Type? T(string name)
    {
        if (Types.TryGetValue(name, out var t)) return t;
        t = Type.GetType(name + ", Stardew Valley");
        if (t == null)
            foreach (var asm in AppDomain.CurrentDomain.GetAssemblies())
                if ((t = asm.GetType(name)) != null) break;
        return Types[name] = t;
    }

    private static object? GetStatic(Type? type, string member)
    {
        if (type == null) return null;
        var p = type.GetProperty(member, Static);
        if (p != null) return p.GetValue(null);
        return type.GetField(member, Static)?.GetValue(null);
    }

    private static void SetStatic(Type? type, string member, object? value)
    {
        if (type == null) return;
        var p = type.GetProperty(member, Static);
        if (p != null && p.CanWrite) { p.SetValue(null, value); return; }
        type.GetField(member, Static)?.SetValue(null, value);
    }

    private static Type? Game1 => T("StardewValley.Game1");

    /// <summary>Game version string (Game1.version), e.g. "1.6.15".</summary>
    public static string GameVersion()
    {
        try { return GetStatic(Game1, "version")?.ToString() ?? ""; }
        catch { return ""; }
    }

    public static object? ActiveMenu
    {
        get => GetStatic(Game1, "activeClickableMenu");
        set => SetStatic(Game1, "activeClickableMenu", value);
    }

    /// <summary>True on the title screen main menu (not inside load/new game submenus).</summary>
    public static bool OnTitleMainScreen()
    {
        var menu = ActiveMenu;
        if (menu == null) return false;
        var title = T("StardewValley.Menus.TitleMenu");
        if (title == null || !title.IsInstanceOfType(menu)) return false;
        return GetStatic(title, "subMenu") == null;
    }

    /// <summary>Close the title screen sub menu (e.g. the load game list, which caches save info).</summary>
    public static void CloseTitleSubMenu()
    {
        try
        {
            var title = T("StardewValley.Menus.TitleMenu");
            if (title != null && title.IsInstanceOfType(ActiveMenu) && GetStatic(title, "subMenu") != null)
                SetStatic(title, "subMenu", null);
        }
        catch { /* best effort */ }
    }

    /// <summary>Show a HUD message (only visible in-world).</summary>
    public static void Hud(string message, bool error = false)
    {
        try
        {
            var hud = T("StardewValley.HUDMessage");
            var ctor = hud?.GetConstructor(new[] { typeof(string), typeof(int) });
            if (ctor == null) return;
            var msg = ctor.Invoke(new object[] { message, error ? 3 : 2 });
            Game1?.GetMethod("addHUDMessage", Static)?.Invoke(null, new[] { msg });
        }
        catch { /* best effort */ }
    }

    /// <summary>Show a yes/no confirmation dialog. Returns false if unavailable.</summary>
    public static bool Confirm(string message, Action onYes, Action onNo)
    {
        try
        {
            var dlgType = T("StardewValley.Menus.ConfirmationDialog");
            var ctor = dlgType?.GetConstructors()
                .FirstOrDefault(c => c.GetParameters() is { Length: >= 3 } ps && ps[0].ParameterType == typeof(string));
            if (ctor == null) return false;
            var ps = ctor.GetParameters();
            var args = new object?[ps.Length];
            args[0] = message;
            args[1] = MakeBehavior(ps[1].ParameterType, onYes);
            args[2] = MakeBehavior(ps[2].ParameterType, onNo);
            for (int i = 3; i < ps.Length; i++) args[i] = ps[i].HasDefaultValue ? ps[i].DefaultValue : null;
            var title = T("StardewValley.Menus.TitleMenu");
            bool onTitle = title != null && title.IsInstanceOfType(ActiveMenu);
            closeDialog = onTitle ? () => SetStatic(title, "subMenu", null) : () => ActiveMenu = null;
            var dlg = ctor.Invoke(args);
            if (onTitle) SetStatic(title, "subMenu", dlg); // keep the TitleMenu as the active menu
            else ActiveMenu = dlg;
            return true;
        }
        catch { return false; }
    }

    private static Action closeDialog = () => { };

    /// <summary>Build a delegate of the game's ConfirmationDialog.behavior type (void(Farmer)).</summary>
    private static Delegate MakeBehavior(Type delegateType, Action action)
    {
        var invoke = delegateType.GetMethod("Invoke")!;
        var parameters = invoke.GetParameters().Select(p => Expression.Parameter(p.ParameterType, p.Name)).ToArray();
        Action wrapped = () =>
        {
            closeDialog(); // ConfirmationDialog doesn't close itself when a callback is set
            action();
        };
        var body = Expression.Invoke(Expression.Constant(wrapped));
        return Expression.Lambda(delegateType, body, parameters).Compile();
    }

    /// <summary>Exit the current save to the title screen without saving.</summary>
    public static void ExitToTitle()
    {
        var m = Game1?.GetMethods(Static).FirstOrDefault(x => x.Name == "ExitToTitle");
        if (m != null)
        {
            m.Invoke(null, m.GetParameters().Select(p => p.HasDefaultValue ? p.DefaultValue : null).ToArray());
            return;
        }
        SetStatic(Game1, "exitToTitle", true);
    }
}
