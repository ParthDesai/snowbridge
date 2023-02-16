pub use frame_support::parameter_types;

#[macro_export]
macro_rules! const_parameter_types {
    (
		$( #[ $attr:meta ] )*
		$vis:vis const $name:ident $(< $($ty_params:ident),* >)?: $type:ty = $value:expr;
		$( $rest:tt )*
	) => (
		$( #[ $attr ] )*
		$vis struct $name $(
			< $($ty_params),* >( $($crate::sp_std::marker::PhantomData<$ty_params>),* )
		)?;
		impl< $($ty_params),* > $name< $($ty_params),* > {
			/// Raw constant to use
			pub const RAW $(< $($ty_params),* >)?: $type = $value;
		}
		parameter_types!(IMPL_CONST $name , $type , $value $( $(, $ty_params)* )?);
		const_parameter_types!( $( $rest )* );
	);
	() => ();
}

#[macro_export]
macro_rules! raw_const {
	($path:path) => ({ use $path as const_st; const_st::RAW });
}
